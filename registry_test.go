package kong_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/alecthomas/assert/v2"

	"github.com/alecthomas/kong"
)

type runtimePluginCmd struct {
	Flag string `short:"f" help:"A plugin flag."`
	ran  bool
}

func (r *runtimePluginCmd) Run() error { r.ran = true; return nil }

type runtimeFuncCmd func() error

func (r runtimeFuncCmd) Run() error { return r() }

type runtimeChildCmd struct {
	Path string `arg help:"Child path argument."`
}

func newRegistryParser(t *testing.T, grammar any, options ...kong.Option) (*kong.Kong, *kong.CommandRegistry, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	options = append([]kong.Option{
		kong.Name("test"),
		kong.RuntimeCommands(),
		kong.Writers(buf, buf),
		kong.Exit(func(int) {}),
	}, options...)
	p, err := kong.New(grammar, options...)
	assert.NoError(t, err)
	reg := p.CommandRegistry()
	assert.NotEqual(t, (*kong.CommandRegistry)(nil), reg)
	return p, reg, buf
}

func registryErrs(t *testing.T, err error) kong.RegistryErrors {
	t.Helper()
	var errs kong.RegistryErrors
	assert.True(t, errors.As(err, &errs), "expected RegistryErrors, got %v", err)
	return errs
}

func hasCode(err error, code kong.RegistryErrorCode) bool {
	return kong.IsRegistryError(err, code)
}

func TestRuntimeRegistryDisabledByDefault(t *testing.T) {
	var cli struct{}
	p, err := kong.New(&cli)
	assert.NoError(t, err)
	assert.Equal(t, (*kong.CommandRegistry)(nil), p.CommandRegistry())
}

func TestRuntimeRegisterAndParse(t *testing.T) {
	var cli struct{}
	p, reg, _ := newRegistryParser(t, &cli)

	res, err := reg.Register("plugin-a", kong.PluginCommand{
		ID:   "migrate",
		Help: "Run migrations.",
		Cmd:  &runtimePluginCmd{},
		Tags: []string{`aliases:"mig"`},
	})
	assert.NoError(t, err)
	assert.Equal(t, uint64(2), res.Generation)
	assert.Equal(t, []kong.CommandIdentity{{Owner: "plugin-a", ID: "migrate"}}, res.Added)

	ctx, err := p.Parse([]string{"migrate", "-f", "value"})
	assert.NoError(t, err)
	selected := ctx.Selected()
	assert.Equal(t, "migrate", selected.Name)
	target := selected.Target.Addr().Interface().(*runtimePluginCmd)
	assert.Equal(t, "value", target.Flag)
	assert.NoError(t, ctx.Run())
	assert.True(t, target.ran)

	// Alias resolves to the same command.
	ctx, err = p.Parse([]string{"mig", "-f", "other"})
	assert.NoError(t, err)
	assert.Equal(t, "migrate", ctx.Selected().Name)
	assert.Equal(t, "other", ctx.Selected().Target.Addr().Interface().(*runtimePluginCmd).Flag)
}

func TestRuntimeTargetsArePrivatePerParse(t *testing.T) {
	var cli struct{}
	p, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Register("p", kong.PluginCommand{ID: "x", Cmd: &runtimePluginCmd{}})
	assert.NoError(t, err)

	ctx1, err := p.Parse([]string{"x", "-f", "one"})
	assert.NoError(t, err)
	ctx2, err := p.Parse([]string{"x"})
	assert.NoError(t, err)

	t1 := ctx1.Selected().Target.Addr().Interface().(*runtimePluginCmd)
	t2 := ctx2.Selected().Target.Addr().Interface().(*runtimePluginCmd)
	assert.NotEqual(t, fmt.Sprintf("%p", t1), fmt.Sprintf("%p", t2))
	assert.Equal(t, "one", t1.Flag)
	assert.Equal(t, "", t2.Flag)
}

func TestRuntimeFuncCommand(t *testing.T) {
	var cli struct{}
	p, reg, _ := newRegistryParser(t, &cli)
	ran := false
	cmd := runtimeFuncCmd(func() error { ran = true; return nil })
	_, err := reg.Register("p", kong.PluginCommand{ID: "go", Cmd: &cmd})
	assert.NoError(t, err)
	ctx, err := p.Parse([]string{"go"})
	assert.NoError(t, err)
	assert.NoError(t, ctx.Run())
	assert.True(t, ran)
}

func TestRuntimeNestedMountAndCrossBatchDependencies(t *testing.T) {
	var cli struct {
		DB struct{} `cmd:"db" help:"Database commands."`
	}
	p, reg, _ := newRegistryParser(t, &cli)

	// Two siblings mounted under a pre-existing static parent.
	res, err := reg.Register("db-plugin",
		kong.PluginCommand{ID: "migrate", Parent: []string{"db"}, Cmd: &runtimePluginCmd{}},
		kong.PluginCommand{ID: "schema", Parent: []string{"db"}, Cmd: &runtimePluginCmd{}},
	)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(res.Added))

	ctx, err := p.Parse([]string{"db", "migrate", "-f", "x"})
	assert.NoError(t, err)
	assert.Equal(t, "db migrate", ctx.Command())
	assert.Equal(t, "x", ctx.Selected().Target.Addr().Interface().(*runtimePluginCmd).Flag)
}

func TestRuntimeUnknownParent(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Register("p", kong.PluginCommand{
		ID: "x", Parent: []string{"nope"}, Cmd: &runtimePluginCmd{},
	})
	assert.True(t, hasCode(err, kong.UnknownParentError), "got %v", err)
}

func TestRuntimeBatchCrossDependencies(t *testing.T) {
	var cli struct{}
	p, reg, _ := newRegistryParser(t, &cli)

	// Child precedes its parent in the batch: both must take effect together.
	_, err := reg.Apply(&kong.BatchChange{
		Owner: "p",
		Register: []kong.PluginCommand{
			{ID: "child", Parent: []string{"parent"}, Cmd: &runtimeChildCmd{}},
			{ID: "parent", Cmd: &runtimePluginCmd{}},
		},
	})
	assert.NoError(t, err)

	ctx, err := p.Parse([]string{"parent", "child", "somewhere"})
	assert.NoError(t, err)
	assert.Equal(t, "parent child <path>", ctx.Command())
	assert.Equal(t, "somewhere", ctx.Selected().Target.Addr().Interface().(*runtimeChildCmd).Path)

	// An existing command cannot be moved beneath itself.
	_, err = reg.Register("p", kong.PluginCommand{
		ID: "parent", Name: "parent", Parent: []string{"parent"}, Cmd: &runtimePluginCmd{},
	})
	assert.True(t, hasCode(err, kong.MountCycleError), "got %v", err)
}

func TestRuntimeBatchFailureIsAtomic(t *testing.T) {
	var cli struct{}
	p, reg, _ := newRegistryParser(t, &cli)

	_, err := reg.Apply(&kong.BatchChange{
		Owner: "p",
		Register: []kong.PluginCommand{
			{ID: "good", Cmd: &runtimePluginCmd{}},
			{ID: "bad id!", Cmd: &runtimePluginCmd{}},
			{ID: "also-good", Cmd: &runtimePluginCmd{}},
		},
	})
	assert.Error(t, err)
	assert.True(t, hasCode(err, kong.InvalidCommandError), "got %v", err)
	assert.Equal(t, uint64(1), reg.Generation())
	assert.Equal(t, 0, len(reg.Registered()))

	// Nothing partially applied: "good" is unknown to the parser.
	_, perr := p.Parse([]string{"good"})
	assert.Error(t, perr)

	// Retry with a fixed batch succeeds.
	_, err = reg.Register("p",
		kong.PluginCommand{ID: "good", Cmd: &runtimePluginCmd{}},
		kong.PluginCommand{ID: "also-good", Cmd: &runtimePluginCmd{}},
	)
	assert.NoError(t, err)
}

func TestRuntimeNameConflictWithStaticAndSibling(t *testing.T) {
	var cli struct {
		Static struct{} `cmd:"static"`
	}
	_, reg, _ := newRegistryParser(t, &cli)

	_, err := reg.Register("p", kong.PluginCommand{ID: "x", Name: "static", Cmd: &runtimePluginCmd{}})
	assert.True(t, hasCode(err, kong.NameConflictError), "got %v", err)

	// Two batch entries claiming the same word.
	_, err = reg.Register("p",
		kong.PluginCommand{ID: "a", Cmd: &runtimePluginCmd{}},
		kong.PluginCommand{ID: "b", Cmd: &runtimePluginCmd{}},
	)
	assert.NoError(t, err)
	_, err = reg.Register("q",
		kong.PluginCommand{ID: "c", Name: "a", Cmd: &runtimePluginCmd{}},
		kong.PluginCommand{ID: "d", Name: "c", Cmd: &runtimePluginCmd{}},
	)
	assert.True(t, hasCode(err, kong.NameConflictError), "got %v", err)
}

func TestRuntimeAliasConflict(t *testing.T) {
	var cli struct {
		One struct{} `cmd:"one"`
	}
	_, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Register("p", kong.PluginCommand{
		ID: "two", Cmd: &runtimePluginCmd{}, Tags: []string{`aliases:"one"`},
	})
	assert.True(t, hasCode(err, kong.AliasConflictError), "got %v", err)
}

func TestRuntimeFlagConflictWithAncestor(t *testing.T) {
	var cli struct {
		DB struct{} `cmd:"db"`
	}
	_, reg, _ := newRegistryParser(t, &cli)

	type migrateCmd struct {
		Help bool `help:"shadow the inherited help flag"`
	}
	_, err := reg.Register("p", kong.PluginCommand{
		ID: "migrate", Parent: []string{"db"}, Cmd: &migrateCmd{},
	})
	assert.True(t, hasCode(err, kong.InvalidCommandError), "got %v", err)
}

func TestRuntimeFlagConflictAcrossBatchParent(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Apply(&kong.BatchChange{
		Owner: "p",
		Register: []kong.PluginCommand{
			{ID: "child", Parent: []string{"parent"}, Cmd: &runtimePluginCmd{}},
			{ID: "parent", Cmd: &runtimePluginCmd{}},
		},
	})
	assert.True(t, hasCode(err, kong.FlagConflictError), "got %v", err)
}

func TestRuntimeDuplicateIdentityInBatch(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)
	target := &runtimePluginCmd{}
	_, err := reg.Register("p",
		kong.PluginCommand{ID: "dup", Cmd: target},
		kong.PluginCommand{ID: "dup", Cmd: &runtimePluginCmd{}},
	)
	assert.True(t, hasCode(err, kong.DuplicateIdentityError), "got %v", err)
}

func TestRuntimeSharedTargetRejected(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)
	shared := &runtimePluginCmd{}
	res, err := reg.Register("p", kong.PluginCommand{ID: "a", Cmd: shared})
	assert.NoError(t, err)
	assert.Equal(t, uint64(2), res.Generation)

	// A second identity in the same batch cannot mount the same target.
	_, err = reg.Register("p",
		kong.PluginCommand{ID: "a", Cmd: &runtimePluginCmd{}},
		kong.PluginCommand{ID: "b", Cmd: shared},
	)
	assert.True(t, hasCode(err, kong.DuplicateTargetError), "got %v", err)

	// Sharing with an already-installed registration is rejected too, and
	// the failed batch changed nothing.
	_, err = reg.Register("q", kong.PluginCommand{ID: "c", Cmd: shared})
	assert.True(t, hasCode(err, kong.DuplicateTargetError), "got %v", err)
	assert.Equal(t, 1, len(reg.Registered()))
}

func TestRuntimeInvalidIdentityAndTags(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)

	_, err := reg.Apply(&kong.BatchChange{
		Owner:    "p",
		Register: []kong.PluginCommand{{ID: "ok", Cmd: &runtimePluginCmd{}, Tags: []string{`bogus:"unterminated`}}},
	})
	assert.True(t, hasCode(err, kong.InvalidTagError), "got %v", err)

	_, err = reg.Register("", kong.PluginCommand{ID: "x", Cmd: &runtimePluginCmd{}})
	assert.True(t, hasCode(err, kong.InvalidBatchError), "got %v", err)
}

func TestRuntimeReplaceResubmitAndUnchanged(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)
	target := &runtimePluginCmd{}
	res, err := reg.Register("p", kong.PluginCommand{ID: "x", Cmd: target})
	assert.NoError(t, err)
	assert.Equal(t, 0, len(res.Replaced))
	assert.Equal(t, uint64(2), res.Generation)

	// Identical resubmission is an idempotent no-op.
	res, err = reg.Register("p", kong.PluginCommand{ID: "x", Cmd: target})
	assert.NoError(t, err)
	assert.Equal(t, uint64(2), res.Generation)
	assert.Equal(t, []kong.CommandIdentity{{Owner: "p", ID: "x"}}, res.Unchanged)

	// Different definition under the same stable identity replaces it.
	res, err = reg.Register("p", kong.PluginCommand{ID: "x", Name: "renamed", Cmd: &runtimePluginCmd{}})
	assert.NoError(t, err)
	assert.Equal(t, uint64(3), res.Generation)
	assert.Equal(t, "p/x", res.Replaced[0].Identity.Owner+"/"+res.Replaced[0].Identity.ID)
	assert.Equal(t, uint64(2), res.Replaced[0].PreviousGeneration)

	found, ok := reg.Find("p", "x")
	assert.True(t, ok)
	assert.Equal(t, "renamed", found.Name)
}

func TestRuntimeUnregisterOwnership(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Register("owner-a", kong.PluginCommand{ID: "x", Cmd: &runtimePluginCmd{}})
	assert.NoError(t, err)

	_, err = reg.Unregister("owner-b", "x")
	assert.True(t, hasCode(err, kong.ForeignOwnerError), "got %v", err)

	_, err = reg.Unregister("owner-a", "missing")
	assert.True(t, hasCode(err, kong.CommandNotFoundError), "got %v", err)

	res, err := reg.Unregister("owner-a", "x")
	assert.NoError(t, err)
	assert.Equal(t, []kong.CommandIdentity{{Owner: "owner-a", ID: "x"}}, res.Removed)
	_, err = reg.Unregister("owner-a", "x")
	assert.True(t, hasCode(err, kong.CommandNotFoundError), "got %v", err)
}

func TestRuntimeDependentRegistrationBlocksUnload(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Register("a", kong.PluginCommand{ID: "parent", Cmd: &runtimePluginCmd{}})
	assert.NoError(t, err)
	_, err = reg.Register("b", kong.PluginCommand{ID: "child", Parent: []string{"parent"}, Cmd: &runtimeChildCmd{}})
	assert.NoError(t, err)

	_, err = reg.Unregister("a", "parent")
	assert.True(t, hasCode(err, kong.DependentRegistrationError), "got %v", err)
	var errs kong.RegistryErrors
	assert.True(t, errors.As(err, &errs))
	assert.Equal(t, "b/child", errs[0].Related[0].Owner+"/"+errs[0].Related[0].ID)

	// Removing both in one batch (each owned by its registrant separately is
	// not possible; unload child first, then parent).
	_, err = reg.Unregister("b", "child")
	assert.NoError(t, err)
	_, err = reg.Unregister("a", "parent")
	assert.NoError(t, err)
}

func TestRuntimeDefaultCommandWiredAfterSplice(t *testing.T) {
	var cli struct {
		Group struct{} `cmd:"group"`
	}
	p, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Register("p", kong.PluginCommand{
		ID: "auto", Parent: []string{"group"}, Cmd: &runtimePluginCmd{},
		Tags: []string{`default:"withargs"`},
	})
	assert.NoError(t, err)

	// Naming the parent selects the registered default command.
	ctx, err := p.Parse([]string{"group", "-f", "v"})
	assert.NoError(t, err)
	assert.Equal(t, "auto", ctx.Selected().Name)
	assert.Equal(t, "v", ctx.Selected().Target.Addr().Interface().(*runtimePluginCmd).Flag)
}

func TestRuntimeGroupAssignment(t *testing.T) {
	var cli struct{}
	p, reg, buf := newRegistryParser(t, &cli, kong.ExplicitGroups([]kong.Group{
		{Key: "plug", Title: "Plugins"},
	}))
	_, err := reg.Register("p", kong.PluginCommand{
		ID: "x", Group: "plug", Help: "grouped", Cmd: &runtimePluginCmd{},
	})
	assert.NoError(t, err)

	_, err = reg.Register("p", kong.PluginCommand{
		ID: "y", Group: "unknown-group", Cmd: &runtimePluginCmd{},
	})
	assert.True(t, hasCode(err, kong.InvalidGroupError), "got %v", err)

	ctx, err := kong.Trace(p, nil)
	assert.NoError(t, err)
	assert.NoError(t, ctx.PrintUsage(false))
	assert.Contains(t, buf.String(), "Plugins")
	ctx.Release()
}

func TestRuntimeDefaultCommandConflict(t *testing.T) {
	var cli struct {
		Group struct{} `cmd:"group"`
	}
	_, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Register("p",
		kong.PluginCommand{ID: "a", Parent: []string{"group"}, Cmd: &runtimePluginCmd{}, Tags: []string{`default:"1"`}},
		kong.PluginCommand{ID: "b", Parent: []string{"group"}, Cmd: &runtimePluginCmd{}, Tags: []string{`default:"1"`}},
	)
	assert.True(t, hasCode(err, kong.DefaultCommandConflictError), "got %v", err)
}

func TestRuntimeSnapshotPinning(t *testing.T) {
	var cli struct{}
	p, reg, buf := newRegistryParser(t, &cli)
	_, err := reg.Register("p", kong.PluginCommand{ID: "old", Cmd: &runtimePluginCmd{}})
	assert.NoError(t, err)

	// Start a trace against the current tree.
	oldCtx, err := kong.Trace(p, []string{})
	assert.NoError(t, err)
	oldLeaves := oldCtx.Snapshot().Leaves(true)
	assert.Equal(t, 1, len(oldLeaves))
	assert.Equal(t, "old", oldLeaves[0].Name)

	// Publish a new tree while the old trace is alive.
	_, err = reg.Register("p", kong.PluginCommand{ID: "new", Cmd: &runtimePluginCmd{}})
	assert.NoError(t, err)

	// The old snapshot is unchanged; a new parse sees the new command.
	assert.Equal(t, 1, len(oldCtx.Snapshot().Leaves(true)))
	ctx, err := p.Parse([]string{"new"})
	assert.NoError(t, err)
	assert.Equal(t, "new", ctx.Selected().Name)

	// Help rendered from the old context does not mention the new command.
	buf.Reset()
	assert.NoError(t, oldCtx.PrintUsage(false))
	assert.NotContains(t, buf.String(), "new")
	assert.Contains(t, buf.String(), "old")

	oldCtx.Release()
}

func TestRuntimeInUseUnloadAndForce(t *testing.T) {
	var cli struct{}
	p, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Register("p", kong.PluginCommand{ID: "x", Cmd: &runtimePluginCmd{}})
	assert.NoError(t, err)

	ctx, err := kong.Trace(p, []string{"x"})
	assert.NoError(t, err)

	_, err = reg.Unregister("p", "x")
	assert.True(t, hasCode(err, kong.CommandsInUseError), "got %v", err)
	var errs kong.RegistryErrors
	assert.True(t, errors.As(err, &errs))
	assert.True(t, errs[0].ActivePins >= 1)

	// Force retires it for new parses while the pinned context keeps working.
	_, err = reg.Apply(&kong.BatchChange{Owner: "p", Remove: []string{"x"}, Force: true})
	assert.NoError(t, err)
	_, err = p.Parse([]string{"x"})
	assert.Error(t, err)
	assert.Equal(t, "x", ctx.Selected().Name)
	ctx.Release()
}

func TestRuntimeHelpReflectsRegistry(t *testing.T) {
	var cli struct{}
	p, reg, buf := newRegistryParser(t, &cli)
	_, err := reg.Register("p",
		kong.PluginCommand{ID: "alpha", Help: "The alpha command.", Cmd: &runtimePluginCmd{}},
		kong.PluginCommand{ID: "beta", Help: "The beta command.", Cmd: &runtimePluginCmd{}},
	)
	assert.NoError(t, err)

	ctx, err := kong.Trace(p, nil)
	assert.NoError(t, err)
	assert.NoError(t, ctx.PrintUsage(false))
	output := buf.String()
	assert.Contains(t, output, "alpha")
	assert.Contains(t, output, "The alpha command.")
	assert.Contains(t, output, "beta")
	ctx.Release()
}

func TestRuntimeCurrentTreeAndLeaves(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)
	_, err := reg.Register("p", kong.PluginCommand{ID: "x", Cmd: &runtimePluginCmd{}})
	assert.NoError(t, err)
	current := reg.Current()
	leaves := current.Leaves(true)
	assert.Equal(t, 1, len(leaves))
	assert.Equal(t, "x", leaves[0].Name)
}

func TestRuntimeLegacyDynamicCommandsProtected(t *testing.T) {
	var cli struct{}
	buf := &bytes.Buffer{}
	p, err := kong.New(&cli,
		kong.RuntimeCommands(),
		kong.DynamicCommand("legacy", "legacy help", "", &runtimePluginCmd{}),
		kong.Writers(buf, buf),
		kong.Exit(func(int) {}),
		kong.Name("test"),
	)
	assert.NoError(t, err)
	reg := p.CommandRegistry()
	assert.NotEqual(t, (*kong.CommandRegistry)(nil), reg)

	// Legacy command parses as before.
	ctx, err := p.Parse([]string{"legacy", "-f", "v"})
	assert.NoError(t, err)
	assert.Equal(t, "v", ctx.Selected().Target.Addr().Interface().(*runtimePluginCmd).Flag)

	// It cannot be replaced at runtime through name collision.
	_, err = reg.Register("p", kong.PluginCommand{ID: "x", Name: "legacy", Cmd: &runtimePluginCmd{}})
	assert.True(t, hasCode(err, kong.NameConflictError), "got %v", err)
}

type lockedWriter struct {
	mu sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(p), nil
}

func TestRuntimeConcurrentAccess(t *testing.T) {
	var cli struct{}
	p, err := kong.New(&cli,
		kong.Name("test"),
		kong.RuntimeCommands(),
		kong.Writers(&lockedWriter{}, &lockedWriter{}),
		kong.Exit(func(int) {}),
	)
	assert.NoError(t, err)
	reg := p.CommandRegistry()

	// Seed stable commands.
	for i := 0; i < 8; i++ {
		_, err := reg.Register(fmt.Sprintf("plugin-%d", i), kong.PluginCommand{
			ID:   fmt.Sprintf("cmd-%d", i),
			Cmd:  &runtimePluginCmd{},
			Help: fmt.Sprintf("command %d", i),
		})
		assert.NoError(t, err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := fmt.Sprintf("tmp-%d-%d", g, i)
				owner := fmt.Sprintf("tmp-%d", g)
				if _, err := reg.Register(owner, kong.PluginCommand{ID: id, Cmd: &runtimePluginCmd{}}); err != nil {
					t.Errorf("register: %v", err)
					return
				}
				ctx, err := p.Parse([]string{id})
				if err != nil {
					t.Errorf("parse: %v", err)
					return
				}
				ctx.Release()
				// Other goroutines may still hold the tree version pinned;
				// force-retire the churn command and rely on snapshots for
				// any in-flight parse.
				if _, err := reg.Apply(&kong.BatchChange{Owner: owner, Remove: []string{id}, Force: true}); err != nil {
					t.Errorf("unregister: %v", err)
					return
				}
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := fmt.Sprintf("cmd-%d", (g+i)%8)
				ctx, err := p.Parse([]string{id})
				if err != nil {
					t.Errorf("stable parse: %v", err)
					return
				}
				if err := ctx.PrintUsage(false); err != nil {
					t.Errorf("help: %v", err)
					return
				}
				ctx.Run()
			}
		}(g)
	}
	wg.Wait()
}

func TestRuntimeRegistryErrorsAreStructured(t *testing.T) {
	var cli struct{}
	_, reg, _ := newRegistryParser(t, &cli)

	_, err := reg.Apply(&kong.BatchChange{
		Owner: "p",
		Register: []kong.PluginCommand{
			{ID: "bad id", Cmd: &runtimePluginCmd{}},
			{ID: "", Cmd: nil},
		},
	})
	var errs kong.RegistryErrors
	assert.True(t, errors.As(err, &errs))
	assert.True(t, len(errs) >= 2, "expected all problems reported at once, got %d: %v", len(errs), err)
	codes := map[kong.RegistryErrorCode]bool{}
	for _, e := range errs {
		codes[e.Code] = true
	}
	assert.True(t, codes[kong.InvalidCommandError], "expected invalid_command among %v", codes)
}
