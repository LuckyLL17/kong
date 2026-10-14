package kong_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alecthomas/assert/v2"

	"github.com/alecthomas/kong"
)

func newPlanKong(t *testing.T, cli any, options ...kong.Option) *kong.Kong {
	t.Helper()
	options = append([]kong.Option{
		kong.Name("test"),
		kong.Exit(func(int) {
			t.Helper()
			t.Fatalf("unexpected exit()")
		}),
	}, options...)
	k, err := kong.New(cli, options...)
	assert.NoError(t, err)
	return k
}

type hookCounter struct {
	beforeReset   atomic.Int32
	beforeResolve atomic.Int32
	beforeApply   atomic.Int32
	afterApply    atomic.Int32
	run           atomic.Int32
}

func (h *hookCounter) BeforeReset() error   { h.beforeReset.Add(1); return nil }
func (h *hookCounter) BeforeResolve() error { h.beforeResolve.Add(1); return nil }
func (h *hookCounter) BeforeApply() error   { h.beforeApply.Add(1); return nil }
func (h *hookCounter) AfterApply() error    { h.afterApply.Add(1); return nil }

type planCLI struct {
	hookCounter
	Debug bool   `short:"d"`
	Level string `default:"info" enum:"debug,info,warn"`
	Name  string `env:"PLAN_TEST_NAME"`
	Tags  []string
	Child struct {
		Opt bool `xor:"pick"`
		Alt bool `xor:"pick"`
	} `kong:"cmd"`
}

func TestPlanPreviewInspection(t *testing.T) {
	var cli planCLI
	k := newPlanKong(t, &cli)

	plan, err := k.Preview([]string{"--debug", "child", "--opt"})
	assert.NoError(t, err)
	assert.Equal(t, kong.PlanStateReady, plan.State())
	assert.Equal(t, "child", plan.Command())
	assert.Equal(t, "child", plan.Selected().Name)
	assert.False(t, plan.HelpRequested())

	// The parse path is exposed and includes root, flag, command and flag traces.
	var kinds []string
	for _, p := range plan.Paths() {
		switch {
		case p.App != nil:
			kinds = append(kinds, "app")
		case p.Flag != nil:
			kinds = append(kinds, "flag:"+p.Flag.Name)
		case p.Command != nil:
			kinds = append(kinds, "cmd:"+p.Command.Name)
		}
	}
	assert.Equal(t, []string{"app", "flag:debug", "cmd:child", "flag:opt"}, kinds)

	byName := map[string]kong.PlannedValue{}
	for _, v := range plan.Values() {
		byName[v.Value.Name] = v
	}
	debug, ok := byName["debug"]
	assert.True(t, ok)
	assert.Equal(t, kong.SourceCLI, debug.Source)
	v, ok := debug.Interface()
	assert.True(t, ok)
	assert.Equal(t, true, v)

	level, ok := byName["level"]
	assert.True(t, ok)
	assert.Equal(t, kong.SourceDefault, level.Source)
	v, _ = level.Interface()
	assert.Equal(t, "info", v)

	// Effective flags along the selected path include both root and child flags.
	flagNames := map[string]kong.PlannedFlag{}
	for _, f := range plan.Flags() {
		flagNames[f.Flag.Name] = f
	}
	_, hasOpt := flagNames["opt"]
	assert.True(t, hasOpt)
	assert.Equal(t, []string{"--debug", "child", "--opt"}, plan.Args())
}

func TestPlanPreviewDoesNotMutateTargetOrRunApplyHooks(t *testing.T) {
	var resetCount, resolveCount, applyCount, afterCount atomic.Int32
	var cli planCLI
	k := newPlanKong(t, &cli,
		kong.WithBeforeReset(func() error { resetCount.Add(1); return nil }),
		kong.WithBeforeResolve(func() error { resolveCount.Add(1); return nil }),
		kong.WithBeforeApply(func() error { applyCount.Add(1); return nil }),
		kong.WithAfterApply(func() error { afterCount.Add(1); return nil }),
	)

	_, err := k.Preview([]string{"--debug", "child", "--opt"})
	assert.NoError(t, err)

	// Nothing was written into the target.
	assert.False(t, cli.Debug)
	assert.Equal(t, "", cli.Level)
	assert.False(t, cli.Child.Opt)

	// Resolution-phase hooks run during preview (on the shadow), application hooks and
	// Run never do.
	assert.True(t, resetCount.Load() > 0)
	assert.True(t, resolveCount.Load() > 0)
	assert.Equal(t, int32(0), applyCount.Load())
	assert.Equal(t, int32(0), afterCount.Load())
	assert.Equal(t, int32(0), cli.run.Load())

	// The Kong instance is still usable: a regular Parse after Preview behaves normally.
	ctx, err := k.Parse([]string{"--debug", "child", "--opt"})
	assert.NoError(t, err)
	assert.Equal(t, "child", ctx.Command())
	assert.True(t, cli.Debug)
	assert.True(t, cli.Child.Opt)
	assert.Equal(t, "info", cli.Level)
}

func TestPlanCommitAppliesAndRunsHooksOnce(t *testing.T) {
	var cli planCLI
	k := newPlanKong(t, &cli)

	plan, err := k.Preview([]string{"--debug", "child", "--alt"})
	assert.NoError(t, err)
	assert.Equal(t, int32(0), cli.beforeApply.Load())
	assert.Equal(t, int32(0), cli.afterApply.Load())

	ctx, err := plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, "child", ctx.Command())
	assert.True(t, cli.Debug)
	assert.True(t, cli.Child.Alt)
	assert.False(t, cli.Child.Opt)
	assert.Equal(t, "info", cli.Level)
	assert.True(t, cli.beforeApply.Load() > 0)
	assert.True(t, cli.afterApply.Load() > 0)
	assert.Equal(t, int32(0), cli.run.Load())

	// Commit is one-shot.
	_, err = plan.Commit()
	assert.True(t, errors.Is(err, kong.ErrPlanCommitted))
}

func TestPlanCancel(t *testing.T) {
	var cli planCLI
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"child", "--opt"})
	assert.NoError(t, err)

	assert.NoError(t, plan.Cancel())
	_, err = k.Commit(plan)
	assert.True(t, errors.Is(err, kong.ErrPlanCancelled))

	// Cancelling leaves the target at its pre-preview state.
	assert.False(t, cli.Child.Opt)
}

func TestPlanForeignKongRejected(t *testing.T) {
	var cli1, cli2 planCLI
	k1 := newPlanKong(t, &cli1)
	k2 := newPlanKong(t, &cli2)
	plan, err := k1.Preview([]string{"child", "--opt"})
	assert.NoError(t, err)
	_, err = k2.Commit(plan)
	assert.True(t, errors.Is(err, kong.ErrPlanForeignKong))
	assert.False(t, cli1.Child.Opt)
}

func TestPlanTargetModificationConflicts(t *testing.T) {
	var cli planCLI
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"child", "--opt"})
	assert.NoError(t, err)

	cli.Debug = true // caller mutates the grammar target after preview

	_, err = plan.Commit()
	assert.True(t, errors.Is(err, kong.ErrPlanTargetModified), err)
	assert.False(t, cli.Child.Opt)
}

func TestPlanEnvironmentChangeConflicts(t *testing.T) {
	t.Setenv("PLAN_TEST_NAME", "preview")
	var cli planCLI
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"child"})
	assert.NoError(t, err)
	nv := planValue(t, plan, "name")
	assert.Equal(t, "preview", nv)

	t.Setenv("PLAN_TEST_NAME", "commit")
	_, err = plan.Commit()
	assert.True(t, errors.Is(err, kong.ErrPlanEnvironmentChanged), err)
	assert.Equal(t, "", cli.Name)
}

type mutableResolver struct {
	value atomic.Value // string
}

func (m *mutableResolver) Validate(*kong.Application) error { return nil }
func (m *mutableResolver) Resolve(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
	if flag.Name != "level" {
		return nil, nil
	}
	v := m.value.Load()
	if v == nil {
		return nil, nil
	}
	return v, nil
}

func planValue(t *testing.T, plan *kong.Plan, name string) any {
	t.Helper()
	for _, v := range plan.Values() {
		if v.Value.Name == name {
			val, ok := v.Interface()
			assert.True(t, ok)
			return val
		}
	}
	t.Fatalf("planned value %q not found", name)
	return nil
}

func TestPlanResolverAppliedAndInvalidated(t *testing.T) {
	resolver := &mutableResolver{}
	var cli planCLI
	k := newPlanKong(t, &cli, kong.Resolvers(resolver))

	resolver.value.Store("resolved-level")
	plan, err := k.Preview([]string{"child"})
	// enum validation rejects "resolved-level"; the raw resolver value is still recorded.
	assert.Error(t, err)
	assert.Equal(t, kong.PlanStateInvalid, plan.State())
	assert.Contains(t, plan.Diagnostics().Error(), "must be one of")

	resolver.value.Store("debug")
	plan2, err := k.Preview([]string{"child"})
	assert.NoError(t, err)
	assert.Equal(t, "debug", planValue(t, plan2, "level"))

	// The resolver changing between preview and commit invalidates the plan.
	resolver.value.Store("warn")
	_, err = plan2.Commit()
	assert.True(t, errors.Is(err, kong.ErrPlanResolverChanged), err)
	assert.Equal(t, "", cli.Level)

	resolver.value.Store("debug")
	plan3, err := k.Preview([]string{"child"})
	assert.NoError(t, err)
	_, err = plan3.Commit()
	assert.NoError(t, err)
	assert.Equal(t, "debug", cli.Level)
}

func TestPlanInvalidDiagnostics(t *testing.T) {
	var cli planCLI
	k := newPlanKong(t, &cli)

	// Bad enum value: trace succeeds, validation fails.
	plan, err := k.Preview([]string{"--level", "nope"})
	assert.Error(t, err)
	assert.Equal(t, kong.PlanStateInvalid, plan.State())
	assert.Contains(t, plan.Diagnostics().Error(), "must be one of")
	_, err = plan.Commit()
	assert.True(t, errors.Is(err, kong.ErrPlanInvalid))

	// Target stayed untouched.
	assert.Equal(t, "", cli.Level)

	// Unknown flag: trace fails, plan carries error diagnostics.
	plan, err = k.Preview([]string{"--nope"})
	assert.Error(t, err)
	assert.Equal(t, kong.PlanStateError, plan.State())
	assert.Error(t, plan.Diagnostics())
	var parseErr *kong.ParseError
	assert.True(t, errors.As(err, &parseErr))
	_, err = plan.Commit()
	assert.True(t, errors.Is(err, kong.ErrPlanInvalid))
}

func TestPlanXorDuplicateAndMissing(t *testing.T) {
	var cli planCLI
	k := newPlanKong(t, &cli)

	plan, err := k.Preview([]string{"child", "--opt", "--alt"})
	assert.Error(t, err)
	assert.Equal(t, kong.PlanStateInvalid, plan.State())
	assert.Contains(t, plan.Diagnostics().Error(), "can't be used together")
	assert.False(t, cli.Child.Opt)
}

func TestPlanLastWins(t *testing.T) {
	var cli struct {
		A string `lastwins:"g"`
		B string `lastwins:"g"`
	}
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"--a", "1", "--b", "2"})
	assert.NoError(t, err)
	values := map[string]any{}
	for _, v := range plan.Values() {
		x, _ := v.Interface()
		values[v.Value.Name] = x
	}
	assert.Equal(t, "", values["a"])
	assert.Equal(t, "2", values["b"])

	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, "", cli.A)
	assert.Equal(t, "2", cli.B)
}

func TestPlanPassthrough(t *testing.T) {
	var cli struct {
		Pass struct {
			Args []string `arg:"" passthrough:""`
		} `kong:"cmd,passthrough"`
	}
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"pass", "--whatever", "-x", "rest"})
	assert.NoError(t, err)
	assert.Equal(t, []string{"--whatever", "-x", "rest"}, planValue(t, plan, "args").([]string))
	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, []string{"--whatever", "-x", "rest"}, cli.Pass.Args)
}

func TestPlanDefaultCommand(t *testing.T) {
	var cli struct {
		Inline struct {
			Value string `arg:""`
		} `kong:"cmd,default:withargs"`
	}
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"inline", "thing"})
	assert.NoError(t, err)
	assert.Equal(t, "inline <value>", plan.Command())
	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, "thing", cli.Inline.Value)
}

type validatedString string

func (v *validatedString) Validate() error {
	if string(*v) == "forbidden" {
		return errors.New("value is forbidden")
	}
	return nil
}

func TestPlanValidatorRunsAtPreviewAndCommit(t *testing.T) {
	var cli struct {
		Value validatedString
	}
	k := newPlanKong(t, &cli)

	plan, err := k.Preview([]string{"--value", "forbidden"})
	assert.Error(t, err)
	assert.Equal(t, kong.PlanStateInvalid, plan.State())
	assert.Contains(t, plan.Diagnostics().Error(), "forbidden")
	_, err = plan.Commit()
	assert.Error(t, err)

	k2cli := struct {
		Value validatedString
	}{}
	k2 := newPlanKong(t, &k2cli)
	plan2, err := k2.Preview([]string{"--value", "ok"})
	assert.NoError(t, err)
	_, err = plan2.Commit()
	assert.NoError(t, err)
	assert.Equal(t, validatedString("ok"), k2cli.Value)
}

func TestPlanRollbackOnHookFailure(t *testing.T) {
	var cli struct {
		Debug bool
		Flag  string
	}
	cli.Flag = "baseline"
	fail := true
	k := newPlanKong(t, &cli, kong.WithAfterApply(func() error {
		if fail {
			return errors.New("boom")
		}
		return nil
	}))
	plan, err := k.Preview([]string{"--debug", "--flag", "committed"})
	assert.NoError(t, err)

	_, err = plan.Commit()
	assert.Error(t, err)
	// Any failure after the write boundary rolls the target back to its pre-preview state.
	assert.False(t, cli.Debug)
	assert.Equal(t, "baseline", cli.Flag)
	assert.Equal(t, kong.PlanStateReady, plan.State())

	// The plan can be retried once the hook is healthy again.
	fail = false
	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.True(t, cli.Debug)
	assert.Equal(t, "committed", cli.Flag)
}

func TestPlanBeforeApplyFailureLeavesTargetUntouched(t *testing.T) {
	var cli struct {
		Flag string
	}
	cli.Flag = "baseline"
	k := newPlanKong(t, &cli, kong.WithBeforeApply(func() error {
		return errors.New("nope")
	}))
	plan, err := k.Preview([]string{"--flag", "x"})
	assert.NoError(t, err)
	_, err = plan.Commit()
	assert.Error(t, err)
	assert.Equal(t, "baseline", cli.Flag)
}

func TestPlanHelpDeferredToCommit(t *testing.T) {
	var cli planCLI
	stdout := &bytes.Buffer{}
	exited := 0
	k := newPlanKong(t, &cli,
		kong.Writers(stdout, io.Discard),
		kong.Exit(func(code int) { exited = code }),
	)

	plan, err := k.Preview([]string{"--help", "child"})
	assert.NoError(t, err)
	assert.True(t, plan.HelpRequested())
	assert.Equal(t, "", stdout.String())
	assert.Equal(t, 0, exited)

	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, 0, exited)
	assert.True(t, strings.Contains(stdout.String(), "Usage: test"))
	// Help exits before any value is written.
	assert.False(t, cli.Child.Alt)

	// The help action, like any commit, runs at most once.
	_, err = plan.Commit()
	assert.True(t, errors.Is(err, kong.ErrPlanCommitted))
}

func TestPlanHelpShortCircuitsValidation(t *testing.T) {
	// In Parse, --help exits from BeforeReset before validation runs: a command-line
	// that would otherwise be missing a required flag still just prints help.
	var cli struct {
		Required string `required:""`
	}
	stdout := &bytes.Buffer{}
	exited := -1
	k := newPlanKong(t, &cli,
		kong.Writers(stdout, io.Discard),
		kong.Exit(func(code int) { exited = code }),
	)
	plan, err := k.Preview([]string{"--help"})
	assert.NoError(t, err)
	assert.Equal(t, kong.PlanStateReady, plan.State())
	assert.True(t, plan.HelpRequested())

	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, 0, exited)
	assert.True(t, strings.Contains(stdout.String(), "Usage: test"))
	assert.Equal(t, "", cli.Required)
}

func TestPlanDynamicCommand(t *testing.T) {
	type plug struct {
		X string
	}
	cmd := &plug{}
	var cli struct {
		Root bool
	}
	k := newPlanKong(t, &cli, kong.DynamicCommand("plug", "dynamic", "", cmd))

	plan, err := k.Preview([]string{"plug", "--x", "yes"})
	assert.NoError(t, err)
	assert.Equal(t, "plug", plan.Command())
	assert.Equal(t, "", cmd.X)

	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, "yes", cmd.X)

	// A second plan refuses when the external dynamic target changed underneath it.
	cmd2 := &plug{}
	k2cli := struct{ Root bool }{}
	k2 := newPlanKong(t, &k2cli, kong.DynamicCommand("plug", "dynamic", "", cmd2))
	plan2, err := k2.Preview([]string{"plug"})
	assert.NoError(t, err)
	cmd2.X = "mutated"
	_, err = k2.Commit(plan2)
	assert.True(t, errors.Is(err, kong.ErrPlanTargetModified), err)
}

func TestPlanContextResolverFromBeforeResolve(t *testing.T) {
	var cli struct {
		hookCounter
		Resolved string
	}
	// A context resolver added by BeforeResolve must be honored by the preview.
	k := newPlanKong(t, &cli, kong.WithBeforeResolve(func(ctx *kong.Context) error {
		ctx.AddResolver(kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
			if flag.Name != "resolved" {
				return nil, nil
			}
			return "from-context", nil
		}))
		return nil
	}))
	plan, err := k.Preview(nil)
	assert.NoError(t, err)
	assert.Equal(t, "from-context", planValue(t, plan, "resolved"))
	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, "from-context", cli.Resolved)
}

func TestPlanCommandAliasesAndNegatable(t *testing.T) {
	var cli struct {
		Verbose bool     `negatable:""`
		Show    struct{} `kong:"cmd" aliases:"ls"`
	}
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"ls", "--no-verbose"})
	assert.NoError(t, err)
	assert.Equal(t, "show", plan.Command())
	assert.Equal(t, false, planValue(t, plan, "verbose"))
	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.False(t, cli.Verbose)
}

func TestPlanPointerCommand(t *testing.T) {
	var cli struct {
		Child *struct {
			X      string
			Nested *struct {
				Y string
			} `kong:"cmd"`
		} `kong:"cmd"`
	}
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"child", "nested", "--y", "deep"})
	assert.NoError(t, err)
	assert.Equal(t, "child nested", plan.Command())
	// Nothing is written to the real target during preview.
	assert.Equal(t, "", cli.Child.Nested.Y)
	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, "deep", cli.Child.Nested.Y)
}

func TestPlanPreviewTwiceDoesNotInterfere(t *testing.T) {
	var cli planCLI
	k := newPlanKong(t, &cli)
	p1, err := k.Preview([]string{"child", "--alt"})
	assert.NoError(t, err)
	p2, err := k.Preview([]string{"child", "--opt"})
	assert.NoError(t, err)
	assert.Equal(t, true, planValue(t, p1, "alt"))
	assert.Equal(t, false, planValue(t, p1, "opt"))
	assert.Equal(t, true, planValue(t, p2, "opt"))
	_, err = p2.Commit()
	assert.NoError(t, err)
	assert.True(t, cli.Child.Opt)
	assert.False(t, cli.Child.Alt)
	// An older plan for a different input cannot be committed afterwards: its target
	// baseline check fails because Commit already wrote new state.
	_, err = p1.Commit()
	assert.True(t, errors.Is(err, kong.ErrPlanTargetModified), err)
}

func TestPlanGrammarChangeDetected(t *testing.T) {
	var cli planCLI
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"child"})
	assert.NoError(t, err)

	// Simulate a grammar snapshot change by editing the model in place.
	k.Model.Node.Children[0].Flags[0].Value.Default = "changed-by-postbuild"

	_, err = plan.Commit()
	assert.True(t, errors.Is(err, kong.ErrPlanGrammarChanged), err)
}

type mapperValue struct {
	decoded string
}

func (m *mapperValue) Decode(ctx *kong.DecodeContext) error {
	var raw string
	if err := ctx.Scan.PopValueInto("m", &raw); err != nil {
		return err
	}
	m.decoded = "mapped:" + raw
	return nil
}

func TestPlanCustomMapperAndAliases(t *testing.T) {
	var cli struct {
		M mapperValue `name:"m" aliases:"mv"`
	}
	k := newPlanKong(t, &cli)
	plan, err := k.Preview([]string{"--mv", "x"})
	assert.NoError(t, err)
	_, err = plan.Commit()
	assert.NoError(t, err)
	assert.Equal(t, "mapped:x", cli.M.decoded)
}

// TestPlanParseParity verifies Preview+Commit produces exactly the same final target
// state as Parse across the full surface of parse semantics.
type parityCLI struct {
	Debug   bool     `short:"d"`
	Level   string   `default:"info" enum:"debug,info,warn"`
	Named   string   `env:"PLAN_PARITY_NAME"`
	Tags    []string `sep:"."`
	Attrs   map[string]string
	Counter int `type:"counter" short:"c"`
	Extra   string
	Child   struct {
		Opt bool   `xor:"pick"`
		Alt bool   `xor:"pick"`
		P   string `and:"pair"`
		Q   string `and:"pair"`
	} `kong:"cmd"`
	Pass struct {
		Args []string `arg:"" passthrough:""`
	} `kong:"cmd,passthrough"`
}

func (*parityCLI) Run() error { return nil }

type parityResolver struct{}

func (parityResolver) Validate(*kong.Application) error { return nil }
func (parityResolver) Resolve(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
	if flag.Name == "extra" {
		return "resolved", nil
	}
	return nil, nil
}

func TestPlanParseParity(t *testing.T) {
	t.Setenv("PLAN_PARITY_NAME", "env-name")
	cases := [][]string{
		{},
		{"-dcc", "--level", "warn", "--tags", "a.b.c", "--attrs", "k=v"},
		{"child", "--opt", "--p", "1", "--q", "2"},
		{"child", "--alt"},
		{"pass", "--raw", "-z", "tail"},
		{"--extra", "ignored-by-cli"},
	}
	for i, args := range cases {
		t.Run(fmt.Sprintf("case%02d", i), func(t *testing.T) {
			var viaParse parityCLI
			kParse := newPlanKong(t, &viaParse, kong.Resolvers(parityResolver{}))
			_, perr := kParse.Parse(args)
			// --extra with an arg is valid; all cases are constructed valid.
			assert.NoError(t, perr)

			var viaPlan parityCLI
			kPlan := newPlanKong(t, &viaPlan, kong.Resolvers(parityResolver{}))
			plan, err := kPlan.Preview(args)
			assert.NoError(t, err)
			_, err = plan.Commit()
			assert.NoError(t, err)

			if !reflect.DeepEqual(viaParse, viaPlan) {
				t.Fatalf("state divergence for %v:\nparse=%#v\nplan =%#v", args, viaParse, viaPlan)
			}
		})
	}
}
