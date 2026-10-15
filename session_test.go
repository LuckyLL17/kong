package kong_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alecthomas/assert/v2"

	"github.com/alecthomas/kong"
)

// --- Grammar shared by the session tests -----------------------------------

type sessionUpper string

func (u *sessionUpper) Decode(ctx *kong.DecodeContext) error {
	token, err := ctx.Scan.PopValue("string")
	if err != nil {
		return err
	}
	*u = sessionUpper(strings.ToUpper(fmt.Sprintf("%v", token.Value)))
	return nil
}

type sessionEmbedFields struct {
	EmbeddedFlag string
}

type sessionPlugin struct {
	PluginFlag string
}

type sessionEmbedOption struct {
	OptFlag string
}

type sessionDynCmd struct {
	DynFlag string
}

func (d *sessionDynCmd) Run(rec *sessionRecorder, conn *sessionConn) error {
	rec.Runs = append(rec.Runs, "dyn:"+d.DynFlag)
	rec.ConnIDs = append(rec.ConnIDs, conn.ID)
	return nil
}

type sessionOneCmd struct {
	One string
}

func (s *sessionOneCmd) BeforeResolve(ctx *kong.Context) error {
	ctx.AddResolver(kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
		if flag.Name == "one" {
			return "ctx-resolved", nil
		}
		return nil, nil
	}))
	return nil
}

func (s *sessionOneCmd) Run(rec *sessionRecorder, conn *sessionConn) error {
	rec.Runs = append(rec.Runs, "one:"+s.One)
	rec.ConnIDs = append(rec.ConnIDs, conn.ID)
	return nil
}

type sessionTwoCmd struct {
	TwoFlag string `required:""`
}

type sessionDefCmd struct {
	DefFlag string
}

func (s *sessionDefCmd) Run(rec *sessionRecorder, conn *sessionConn) error {
	rec.Runs = append(rec.Runs, "def:"+s.DefFlag)
	rec.ConnIDs = append(rec.ConnIDs, conn.ID)
	return nil
}

type sessionPipeCmd struct {
	Args []string `arg:"" passthrough:""`
}

type sessionRoot struct {
	Glob string
	Ptr  *string
	Num  int  `kong:"default='5'"`
	No   bool `kong:"negatable"`
	Tags []string
	Attr map[string]string
	Up   sessionUpper

	LWA string `kong:"lastwins='lw'"`
	LWB string `kong:"lastwins='lw'"`

	Embedded sessionEmbedFields `embed:""`

	One  sessionOneCmd  `kong:"cmd"`
	Two  sessionTwoCmd  `kong:"cmd"`
	Def  sessionDefCmd  `kong:"cmd,default"`
	Pipe sessionPipeCmd `kong:"cmd,pass"`

	kong.Plugins
}

func (s *sessionRoot) AfterApply(rec *sessionRecorder) error {
	rec.Runs = append(rec.Runs, "afterapply")
	return nil
}

type sessionConn struct{ ID int }

var sessionConnSeq int64

func newSessionConn() *sessionConn {
	return &sessionConn{ID: int(atomic.AddInt64(&sessionConnSeq, 1))}
}

type sessionRecorder struct {
	Runs     []string
	ConnIDs  []int
	ExitCode int
}

func newSessionGrammar(t *testing.T, cli *sessionRoot, opts ...kong.Option) *kong.Kong {
	t.Helper()
	resolver := kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
		if flag.Name == "glob" {
			return "from-global-resolver", nil
		}
		return nil, nil
	})
	defaults := []kong.Option{
		kong.Name("sessiontest"),
		kong.Exit(func(int) {}),
		kong.Resolvers(resolver),
		kong.BindSingletonProvider(newSessionConn),
		kong.Embed(&sessionEmbedOption{}),
		kong.DynamicCommand("dyn", "Dynamic command", "", &sessionDynCmd{}),
	}
	p, err := kong.New(cli, append(defaults, opts...)...)
	assert.NoError(t, err)
	return p
}

type testSession struct {
	*kong.Session
	Stdout   *bytes.Buffer
	Stderr   *bytes.Buffer
	Recorder *sessionRecorder
}

func newTestSession(t *testing.T, p *kong.Kong, opts ...kong.SessionOption) *testSession {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rec := &sessionRecorder{}
	opts = append([]kong.SessionOption{
		kong.SessionWriters(stdout, stderr),
		kong.SessionExit(func(code int) { rec.ExitCode = code }),
		kong.SessionBind(rec),
	}, opts...)
	s, err := p.NewSession(opts...)
	assert.NoError(t, err)
	return &testSession{Session: s, Stdout: stdout, Stderr: stderr, Recorder: rec}
}

func sessionTarget[T any](t *testing.T, ts *testSession) *T {
	t.Helper()
	typed, ok := ts.Target().(*T)
	assert.True(t, ok)
	return typed
}

// --- Tests -----------------------------------------------------------------

func TestSessionIndependentParseTargets(t *testing.T) {
	var cli sessionRoot
	p := newSessionGrammar(t, &cli)

	ts1 := newTestSession(t, p)
	ctx1, err := ts1.Parse([]string{"--ptr=hello", "--tags=a,b", "--attr=k=v", "--up=hi", "one"})
	assert.NoError(t, err)
	assert.Equal(t, "one", ctx1.Command())
	assert.NoError(t, ctx1.Run())

	r1 := sessionTarget[sessionRoot](t, ts1)
	assert.Equal(t, "hello", *r1.Ptr)
	assert.Equal(t, []string{"a", "b"}, r1.Tags)
	assert.Equal(t, map[string]string{"k": "v"}, r1.Attr)
	assert.Equal(t, sessionUpper("HI"), r1.Up)
	assert.Equal(t, "ctx-resolved", r1.One.One)
	assert.Equal(t, "from-global-resolver", r1.Glob)
	assert.Equal(t, 5, r1.Num)
	assert.Contains(t, strings.Join(ts1.Recorder.Runs, ","), "one:ctx-resolved")
	assert.Equal(t, "", ts1.Stdout.String())

	ts2 := newTestSession(t, p)
	_, err = ts2.Parse([]string{"two", "--two-flag=x", "--no-no", "--lwb=win"})
	assert.NoError(t, err)
	r2 := sessionTarget[sessionRoot](t, ts2)
	assert.Equal(t, "x", r2.Two.TwoFlag)
	assert.False(t, r2.No)
	assert.Equal(t, "", r2.LWA)
	assert.Equal(t, "win", r2.LWB)

	// Session 1 must not see any of session 2's parse.
	assert.Equal(t, "", r1.LWA)
	assert.Equal(t, "", r1.LWB)
	assert.Equal(t, "", r1.Two.TwoFlag)
	assert.False(t, r1.No)
	assert.Equal(t, []string{"a", "b"}, r1.Tags)

	// The grammar originally passed to New() must remain untouched.
	assert.Equal(t, (*string)(nil), cli.Ptr)
	assert.Equal(t, "", cli.Glob)
	assert.Equal(t, 0, cli.Num)
}

func TestSessionLastWinsStateIsolation(t *testing.T) {
	var cli sessionRoot
	p := newSessionGrammar(t, &cli)

	ts1 := newTestSession(t, p)
	_, err := ts1.Parse([]string{"--lwa=first", "--lwb=second"})
	assert.NoError(t, err)
	r1 := sessionTarget[sessionRoot](t, ts1)
	assert.Equal(t, "", r1.LWA)
	assert.Equal(t, "second", r1.LWB)

	ts2 := newTestSession(t, p)
	_, err = ts2.Parse([]string{"--lwa=solo"})
	assert.NoError(t, err)
	r2 := sessionTarget[sessionRoot](t, ts2)
	assert.Equal(t, "solo", r2.LWA)
	assert.Equal(t, "", r2.LWB)

	ts3 := newTestSession(t, p)
	_, err = ts3.Parse([]string{"--lwb=only"})
	assert.NoError(t, err)
	r3 := sessionTarget[sessionRoot](t, ts3)
	assert.Equal(t, "", r3.LWA)
	assert.Equal(t, "only", r3.LWB)
}

func TestSessionDefaultCommandPassthroughAndExtras(t *testing.T) {
	var cli sessionRoot
	cli.Plugins = kong.Plugins{&sessionPlugin{}}
	p := newSessionGrammar(t, &cli)

	// Empty args select the default command.
	ts1 := newTestSession(t, p)
	ctx1, err := ts1.Parse([]string{})
	assert.NoError(t, err)
	assert.Equal(t, "def", ctx1.Command())
	assert.NoError(t, ctx1.Run())
	assert.Contains(t, strings.Join(ts1.Recorder.Runs, ","), "def:")

	// Passthrough command keeps raw args.
	ts2 := newTestSession(t, p)
	_, err = ts2.Parse([]string{"pipe", "--raw", "-x", "rest"})
	assert.NoError(t, err)
	assert.Equal(t, []string{"--raw", "-x", "rest"}, sessionTarget[sessionRoot](t, ts2).Pipe.Args)

	// Embedded struct, Embed() option and Plugins all retarget correctly.
	ts3 := newTestSession(t, p)
	_, err = ts3.Parse([]string{"--embedded-flag=e", "--opt-flag=o", "--plugin-flag=pl"})
	assert.NoError(t, err)
	r3 := sessionTarget[sessionRoot](t, ts3)
	assert.Equal(t, "e", r3.Embedded.EmbeddedFlag)
	plugin, ok := r3.Plugins[0].(*sessionPlugin)
	assert.True(t, ok)
	assert.Equal(t, "pl", plugin.PluginFlag)

	ts4 := newTestSession(t, p)
	ctx4, err := ts4.Parse([]string{"dyn", "--dyn-flag=d"})
	assert.NoError(t, err)
	assert.NoError(t, ctx4.Run())
	assert.Contains(t, strings.Join(ts4.Recorder.Runs, ","), "dyn:d")
}

func TestSessionFailureAndHelpDoNotLeak(t *testing.T) {
	var cli sessionRoot
	p := newSessionGrammar(t, &cli)

	// Validation failure in one session.
	ts1 := newTestSession(t, p)
	_, err := ts1.Parse([]string{"two"})
	assert.Error(t, err)

	// Help in another session goes to its own writer and invokes its Exit.
	ts2 := newTestSession(t, p)
	_, err = ts2.Parse([]string{"--help"})
	assert.Error(t, err) // noop Exit means validation still reports the missing command
	assert.Equal(t, 0, ts2.Recorder.ExitCode)
	assert.Contains(t, ts2.Stdout.String(), "Usage: sessiontest")
	assert.Contains(t, ts2.Stdout.String(), "--glob")

	// A third session parses cleanly despite the failure and help above.
	ts3 := newTestSession(t, p)
	_, err = ts3.Parse([]string{"one"})
	assert.NoError(t, err)
	assert.Equal(t, "", ts3.Stdout.String())
	assert.Equal(t, "", ts3.Stderr.String())
	assert.Equal(t, "ctx-resolved", sessionTarget[sessionRoot](t, ts3).One.One)
}

func TestSessionTraceResolveValidateCallRun(t *testing.T) {
	var cli sessionRoot
	p := newSessionGrammar(t, &cli)

	manualResolver := kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
		if flag.Name == "one" {
			return "manual-resolved", nil
		}
		return nil, nil
	})
	ts := newTestSession(t, p, kong.SessionResolvers(manualResolver))
	ctx, err := ts.Trace([]string{"one"})
	assert.NoError(t, err)
	assert.Equal(t, error(nil), ctx.Error)
	// Trace alone must not apply anything to the target.
	assert.Equal(t, "", sessionTarget[sessionRoot](t, ts).One.One)
	assert.NoError(t, ctx.Reset())
	assert.NoError(t, ctx.Resolve())
	assert.NoError(t, ctx.Validate())
	_, err = ctx.Apply()
	assert.NoError(t, err)
	assert.Equal(t, "manual-resolved", sessionTarget[sessionRoot](t, ts).One.One)

	// Call uses session bindings and the session-scoped singleton provider.
	out, err := ctx.Call(func(rec *sessionRecorder, conn *sessionConn) string {
		return fmt.Sprintf("conn=%d", conn.ID)
	})
	assert.NoError(t, err)
	callResult, ok := out[0].(string)
	assert.True(t, ok)
	assert.True(t, strings.HasPrefix(callResult, "conn="))

	// Run sees the same singleton connection for this session.
	assert.NoError(t, ctx.Run())
}

func TestSessionSingletonProvidersAreScopedPerSession(t *testing.T) {
	var cli sessionRoot
	p := newSessionGrammar(t, &cli)

	ts1 := newTestSession(t, p)
	ctx1, err := ts1.Parse([]string{"one"})
	assert.NoError(t, err)
	assert.NoError(t, ctx1.Run())

	ts2 := newTestSession(t, p)
	ctx2, err := ts2.Parse([]string{"def"})
	assert.NoError(t, err)
	assert.NoError(t, ctx2.Run())

	assert.Equal(t, 1, len(ts1.Recorder.ConnIDs))
	assert.Equal(t, 1, len(ts2.Recorder.ConnIDs))
	assert.NotEqual(t, ts1.Recorder.ConnIDs[0], ts2.Recorder.ConnIDs[0])
}

func TestSessionResolversAndWritersAreIndependent(t *testing.T) {
	var cli sessionRoot
	p := newSessionGrammar(t, &cli)

	sessionResolver := kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
		if flag.Name == "glob" {
			return "session-resolver", nil
		}
		return nil, nil
	})
	ts1 := newTestSession(t, p, kong.SessionResolvers(sessionResolver))
	_, err := ts1.Parse([]string{})
	assert.NoError(t, err)
	assert.Equal(t, "session-resolver", sessionTarget[sessionRoot](t, ts1).Glob)
	assert.Equal(t, "", ts1.Stdout.String())

	ts2 := newTestSession(t, p)
	_, err = ts2.Parse([]string{})
	assert.NoError(t, err)
	assert.Equal(t, "from-global-resolver", sessionTarget[sessionRoot](t, ts2).Glob)
}

func TestSessionBaseParserStillUsableAfterSessions(t *testing.T) {
	var cli sessionRoot
	stdout := &bytes.Buffer{}
	p := newSessionGrammar(t, &cli, kong.Writers(stdout, stdout), kong.Bind(&sessionRecorder{}))

	for i := 0; i < 3; i++ {
		ts := newTestSession(t, p)
		_, err := ts.Parse([]string{"one"})
		assert.NoError(t, err)
	}

	// Direct single-instance use keeps its semantics and its own target.
	ctx, err := p.Parse([]string{"--glob=direct", "one"})
	assert.NoError(t, err)
	assert.Equal(t, "one", ctx.Command())
	assert.Equal(t, "direct", cli.Glob)
	assert.Equal(t, "ctx-resolved", cli.One.One)
}

func TestSessionBaseGrammarModelStaysPristine(t *testing.T) {
	var cli sessionRoot
	p := newSessionGrammar(t, &cli)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := newTestSession(t, p)
			switch i % 6 {
			case 0:
				_, _ = ts.Parse([]string{"one"})
			case 1:
				_, _ = ts.Parse([]string{"two", "--two-flag=x"})
			case 2:
				_, _ = ts.Parse([]string{"--help"})
			case 3:
				_, _ = ts.Parse([]string{"bogus-command"})
			case 4:
				_, _ = ts.Parse([]string{"--lwa=a", "--lwb=b"})
			case 5:
				ctx, _ := ts.Trace([]string{"def"})
				_ = ctx.PrintUsage(true)
			}
		}(i)
	}
	wg.Wait()

	pristine := true
	_ = kong.Visit(p.Model, func(v kong.Visitable, next kong.Next) error {
		switch v := v.(type) {
		case *kong.Flag:
			if v.Set {
				pristine = false
			}
		case *kong.Value:
			if v.Set || v.Active {
				pristine = false
			}
		case *kong.Node:
			if v.Active {
				pristine = false
			}
		}
		return next(nil)
	})
	assert.True(t, pristine)
}

type sessionPtrCmd struct {
	Value string
}

type sessionBranchCLI struct {
	User struct {
		ID struct {
			ID   string   `kong:"arg"`
			Drop struct{} `kong:"cmd"`
		} `kong:"arg"`
	} `kong:"cmd"`
	Ptr *sessionPtrCmd `kong:"cmd"`
}

func TestSessionBranchingArgumentAndPointerCommand(t *testing.T) {
	var cli sessionBranchCLI
	p := mustNew(t, &cli)

	ts1 := newTestSession(t, p)
	ctx1, err := ts1.Parse([]string{"user", "42", "drop"})
	assert.NoError(t, err)
	assert.Equal(t, "user <id> drop", ctx1.Command())
	r1 := sessionTarget[sessionBranchCLI](t, ts1)
	assert.Equal(t, "42", r1.User.ID.ID)

	ts2 := newTestSession(t, p)
	_, err = ts2.Parse([]string{"ptr", "--value=v"})
	assert.NoError(t, err)
	r2 := sessionTarget[sessionBranchCLI](t, ts2)
	assert.Equal(t, "v", r2.Ptr.Value)

	// The pointer command is hydrated by Kong, but each session owns a distinct
	// allocation and session 1 never sees session 2's value.
	assert.NotEqual(t, r1.Ptr, r2.Ptr)
	assert.Equal(t, "", r1.Ptr.Value)

	// Original grammar stays untouched.
	assert.Equal(t, "", cli.Ptr.Value)
	assert.Equal(t, "", cli.User.ID.ID)
}

type sessionMapperCLI struct {
	Custom string
}

type sessionQuotedMapper struct{}

func (sessionQuotedMapper) Decode(ctx *kong.DecodeContext, target reflect.Value) error {
	token, err := ctx.Scan.PopValue("string")
	if err != nil {
		return err
	}
	target.SetString("[" + fmt.Sprintf("%v", token.Value) + "]")
	return nil
}

func TestSessionCustomValueMapper(t *testing.T) {
	var cli sessionMapperCLI
	p := mustNew(t, &cli, kong.ValueMapper(&cli.Custom, sessionQuotedMapper{}))

	ts1 := newTestSession(t, p)
	_, err := ts1.Parse([]string{"--custom=hello"})
	assert.NoError(t, err)
	assert.Equal(t, "[hello]", sessionTarget[sessionMapperCLI](t, ts1).Custom)

	ts2 := newTestSession(t, p)
	_, err = ts2.Parse([]string{"--custom=world"})
	assert.NoError(t, err)
	assert.Equal(t, "[world]", sessionTarget[sessionMapperCLI](t, ts2).Custom)
	assert.Equal(t, "[hello]", sessionTarget[sessionMapperCLI](t, ts1).Custom)
}

type sessionFuncCmd func() error

func (cf sessionFuncCmd) Run() error { return cf() }

func TestSessionFuncDynamicCommand(t *testing.T) {
	var cli struct {
		One struct{} `kong:"cmd,one"`
	}
	ran := false
	fn := sessionFuncCmd(func() error { ran = true; return nil })
	p := mustNew(t, &cli, kong.DynamicCommand("four", "", "", &fn))

	ts1 := newTestSession(t, p)
	ctx, err := ts1.Parse([]string{"four"})
	assert.NoError(t, err)
	assert.Equal(t, "four", ctx.Command())
	assert.NoError(t, ctx.Run())
	assert.True(t, ran)
	assert.Equal(t, "", ts1.Stdout.String())
	assert.Equal(t, "", ts1.Stderr.String())

	// Help still renders the dynamic command without affecting other sessions.
	ts2 := newTestSession(t, p)
	_, err = ts2.Parse([]string{"--help"})
	assert.Error(t, err)
	assert.Contains(t, ts2.Stdout.String(), "four")
}

func TestSessionSubcommandHelp(t *testing.T) {
	var cli sessionRoot
	p := newSessionGrammar(t, &cli)

	ts := newTestSession(t, p)
	ctx, err := ts.Parse([]string{"one", "--help"})
	assert.NoError(t, err)
	assert.Equal(t, "one", ctx.Selected().Name)
	assert.Contains(t, ts.Stdout.String(), "Usage: sessiontest one")
	assert.Equal(t, 0, ts.Recorder.ExitCode)
}

func TestSessionConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	assert.NoError(t, os.WriteFile(path, []byte(`{"glob":"from-config"}`), 0o600))

	var cli sessionRoot
	p := newSessionGrammar(t, &cli, kong.Configuration(kong.JSON, path))

	ts1 := newTestSession(t, p, kong.SessionResolvers(kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
		if flag.Name == "glob" {
			return "session-wins", nil
		}
		return nil, nil
	})))
	_, err := ts1.Parse([]string{})
	assert.NoError(t, err)
	// Session resolvers are consulted after the configuration resolver and win.
	assert.Equal(t, "session-wins", sessionTarget[sessionRoot](t, ts1).Glob)

	ts2 := newTestSession(t, p)
	_, err = ts2.Parse([]string{})
	assert.NoError(t, err)
	assert.Equal(t, "from-config", sessionTarget[sessionRoot](t, ts2).Glob)
}

// exerciseConcurrentSession drives one mixed scenario against a fresh session.
func exerciseConcurrentSession(i int, s *kong.Session) (mayError bool, err error) {
	var ctx *kong.Context
	switch i % 7 {
	case 0:
		ctx, err = s.Parse([]string{"one"})
		if err == nil {
			err = ctx.Run()
		}
	case 1:
		ctx, err = s.Parse([]string{"def"})
		if err == nil {
			err = ctx.Run()
		}
	case 2:
		ctx, err = s.Parse([]string{"two", "--two-flag=ok"})
	case 3:
		_, err = s.Parse([]string{"--help"})
	case 4:
		_, err = s.Parse([]string{"this-command-does-not-exist"})
	case 5:
		ctx, _ = s.Trace([]string{"one"})
		if ctx != nil && ctx.Error == nil {
			err = ctx.PrintUsage(false)
		}
	case 6:
		ctx, err = s.Parse([]string{"dyn", "--dyn-flag=z"})
		if err == nil {
			err = ctx.Run()
		}
	}
	if i%7 == 0 {
		root, ok := s.Target().(*sessionRoot)
		if ctx == nil || !ok || root.One.One != "ctx-resolved" {
			err = errSessionScenario
		}
	}
	return i%7 == 3 || i%7 == 4, err
}

var errSessionScenario = errors.New("concurrent session scenario failed")

func TestSessionConcurrentCreateParseHelpAndRun(t *testing.T) {
	var cli sessionRoot
	p := newSessionGrammar(t, &cli)

	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	var failures int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			rec := &sessionRecorder{}
			s, err := p.NewSession(
				kong.SessionWriters(stdout, stderr),
				kong.SessionExit(func(int) {}),
				kong.SessionBind(rec),
			)
			if err != nil {
				atomic.AddInt64(&failures, 1)
				return
			}
			mayError, err := exerciseConcurrentSession(i, s)
			if !mayError && err != nil {
				atomic.AddInt64(&failures, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	assert.Equal(t, int64(0), atomic.LoadInt64(&failures))
}
