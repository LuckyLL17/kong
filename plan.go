package kong

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"reflect"
	"strings"
	"unsafe"
)

// PlanState describes the lifecycle state of a [Plan].
type PlanState int

const (
	// PlanStateReady means the plan traced and validated successfully and can be committed.
	PlanStateReady PlanState = iota
	// PlanStateInvalid means the command-line traced, but validation reported diagnostics.
	// The plan is inspectable but cannot be committed.
	PlanStateInvalid
	// PlanStateError means the command-line could not even be traced. The plan is inspectable
	// for diagnostics, but carries no committable state.
	PlanStateError
	// PlanStateCommitted means the plan was applied to the grammar target exactly once.
	PlanStateCommitted
	// PlanStateCancelled means the plan was explicitly cancelled.
	PlanStateCancelled
)

func (s PlanState) String() string {
	switch s {
	case PlanStateReady:
		return "ready"
	case PlanStateInvalid:
		return "invalid"
	case PlanStateError:
		return "error"
	case PlanStateCommitted:
		return "committed"
	case PlanStateCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// Sentinel errors reported when a plan is rejected at commit time.
var (
	// ErrPlanInvalid is returned when committing a plan that did not validate successfully.
	ErrPlanInvalid = errors.New("kong: plan is not valid")
	// ErrPlanCommitted is returned when a plan is committed more than once.
	ErrPlanCommitted = errors.New("kong: plan already committed")
	// ErrPlanCancelled is returned when a cancelled plan is committed.
	ErrPlanCancelled = errors.New("kong: plan was cancelled")
	// ErrPlanForeignKong is returned when a plan produced by one [Kong] instance is
	// committed against a different one.
	ErrPlanForeignKong = errors.New("kong: plan belongs to a different Kong instance")
	// ErrPlanGrammarChanged is returned when the grammar snapshot (including dynamic
	// commands) no longer matches the one the plan was built from.
	ErrPlanGrammarChanged = errors.New("kong: grammar changed since the plan was created")
	// ErrPlanResolverChanged is returned when a resolver would now provide a different
	// value than the one recorded in the plan.
	ErrPlanResolverChanged = errors.New("kong: resolver results changed since the plan was created")
	// ErrPlanEnvironmentChanged is returned when an environment variable used by the
	// plan changed between preview and commit.
	ErrPlanEnvironmentChanged = errors.New("kong: environment changed since the plan was created")
	// ErrPlanTargetModified is returned when the grammar target struct was modified
	// between preview and commit.
	ErrPlanTargetModified = errors.New("kong: grammar target was modified since the plan was created")
)

// ValueSource identifies where a planned value originated from.
type ValueSource int

const (
	// SourceZero means the value was neither given nor defaulted, so the zero value applies.
	SourceZero ValueSource = iota
	// SourceDefault means the value comes from the "default" tag.
	SourceDefault
	// SourceEnv means the value comes from an environment variable.
	SourceEnv
	// SourceResolver means the value comes from a [Resolver] (including Configuration files).
	SourceResolver
	// SourceCLI means the value was parsed from the command-line arguments.
	SourceCLI
)

func (s ValueSource) String() string {
	switch s {
	case SourceDefault:
		return "default"
	case SourceEnv:
		return "env"
	case SourceResolver:
		return "resolver"
	case SourceCLI:
		return "cli"
	case SourceZero:
		return "zero"
	default:
		return "unknown"
	}
}

// PlannedValue is a single value that a [Plan] will apply to the grammar target.
type PlannedValue struct {
	// Value is the grammar value the final value belongs to.
	Value *Value
	// Source records how the final value was determined.
	Source ValueSource

	final reflect.Value
}

// Reflect returns the final value as a [reflect.Value].
func (p PlannedValue) Reflect() reflect.Value { return p.final }

// Interface returns the final Go value. The boolean is false for values that cannot
// be interfaced (values reached through unexported struct fields); use [PlannedValue.Reflect]
// in that case.
func (p PlannedValue) Interface() (any, bool) {
	if !p.final.IsValid() {
		return nil, false
	}
	return safeInterface(p.final)
}

// PlannedFlag is a flag effective along the selected command path.
type PlannedFlag struct {
	// Flag is the grammar flag.
	Flag *Flag
	// Source records how the effective value was determined.
	Source ValueSource

	value reflect.Value
}

// Reflect returns the effective value as a [reflect.Value].
func (p PlannedFlag) Reflect() reflect.Value { return p.value }

// Interface returns the effective Go value. The boolean is false when the value
// cannot be interfaced.
func (p PlannedFlag) Interface() (any, bool) {
	if !p.value.IsValid() {
		return nil, false
	}
	return safeInterface(p.value)
}

// Set reports whether the flag was explicitly supplied (via the command-line, an
// environment variable or a resolver).
func (p PlannedFlag) Set() bool {
	return p.Source == SourceCLI || p.Source == SourceEnv || p.Source == SourceResolver
}

// Plan is an inspectable two-phase parse result.
//
// A Plan is produced by [Kong.Preview]. It records the selected command, the effective
// flags and values that would be written, the full parse path and any diagnostics. The
// grammar target is not modified by a preview and no Run/BeforeApply/AfterApply hooks
// run during one.
//
// Apply the plan exactly once with [Kong.Commit] (or [Plan.Commit]), or discard it with
// [Plan.Cancel]. A plan is bound to the [Kong] instance, grammar snapshot, arguments,
// resolver results, environment and target state it was created from; any change causes
// commit to fail with a wrapped sentinel error.
type Plan struct {
	k    *Kong
	args []string
	ctx  *Context

	state PlanState
	diag  error

	grammar uint64
	env     map[string]envState

	// Deep, independent snapshots of the grammar root.
	baselineRoot reflect.Value // target state as it was when the plan was created
	resetRoot    reflect.Value // state after Reset/Resolve, before Apply

	// Address maps chaining the original graph to the shadow graph used during preview.
	mOrigShadow    addrMap // grammar root allocations
	mOrigShadowExt addrMap // external (dynamic command, ...) allocations
	mShadowReset   addrMap // shadow root -> reset root
	mShadowResetEx addrMap // shadow external -> reset external

	// Targets that live outside the grammar root allocation (dynamic commands, the
	// built-in help flag, ...).
	externals map[uintptr]*externalTarget

	// Resolver observations captured while resolving the plan.
	resolverResults map[*Flag]resolverObservation
	contextResolver []Resolver

	// Model metadata state after a successful preview pipeline, restored after commit
	// to match the observable state Parse leaves behind.
	postMeta *modelMeta

	// help is set when the command-line requested the built-in help action. In Parse
	// this exits before Reset/Resolve/Apply/Validate, so the plan stops at the same
	// boundary.
	help bool

	values []PlannedValue
	flags  []PlannedFlag
}

type resolverObservation struct {
	queried  bool
	selected any
}

type envState struct {
	set   bool
	value string
}

type externalTarget struct {
	orig     reflect.Value
	baseline reflect.Value
	shadow   reflect.Value
	reset    reflect.Value

	// mShadowReset translates shadow allocation addresses for this target to those
	// of reset.
	mShadowReset addrMap
}

// externalAllocator hands out independent shadow storage for targets outside the
// grammar root allocation (dynamic command structs, the built-in help flag, ...).
// Allocations are recorded as address ranges, so fields nested in a shadowed struct
// (including fields behind pointers and slices) resolve to the same shadow graph.
type externalAllocator struct {
	ranges  addrMap
	targets map[uintptr]*externalTarget
}

func newExternalAllocator() *externalAllocator {
	return &externalAllocator{
		ranges:  addrMap{},
		targets: map[uintptr]*externalTarget{},
	}
}

// miss allocates a fresh shadow clone (recording every nested allocation) and
// registers the root range.
func (a *externalAllocator) miss(orig reflect.Value) reflect.Value {
	if shadow, ok := a.ranges.translate(orig); ok {
		return shadow
	}
	base := orig.UnsafeAddr()
	shadow := reflect.New(orig.Type()).Elem()
	//nolint:gosec // allocation range recorded for shadow address translation.
	a.ranges = append(a.ranges, allocRange{
		old:  base,
		ptr:  unsafe.Pointer(shadow.UnsafeAddr()),
		size: orig.Type().Size(),
	})
	copyValue(shadow, orig, &a.ranges)
	if _, seen := a.targets[base]; !seen {
		a.targets[base] = &externalTarget{
			orig:     orig,
			baseline: standaloneClone(orig),
			shadow:   shadow,
		}
	}
	return shadow
}

// State returns the plan lifecycle state.
func (p *Plan) State() PlanState { return p.state }

// Diagnostics returns the error recorded while tracing or validating the plan, or nil
// when the plan is ready to commit.
func (p *Plan) Diagnostics() error { return p.diag }

// Args returns the command-line arguments the plan was built from.
func (p *Plan) Args() []string { return append([]string(nil), p.args...) }

// Context returns the parse [Context] backing the plan. It exposes the parse path,
// selected command, available flags, remainder tokens and so on. The context is never
// applied to the real grammar target by a preview.
func (p *Plan) Context() *Context { return p.ctx }

// Selected returns the selected command or argument node, mirroring [Context.Selected].
func (p *Plan) Selected() *Node { return p.ctx.Selected() }

// Command returns the full command path, mirroring [Context.Command].
func (p *Plan) Command() string { return p.ctx.Command() }

// Paths returns the recorded parse path.
func (p *Plan) Paths() []*Path { return p.ctx.Path }

// HelpRequested reports whether the built-in help flag was present on the command-line.
//
// The help action (printing help, invoking the configured exit function) is suppressed
// during preview; it runs at commit time, before any value is written.
func (p *Plan) HelpRequested() bool {
	for _, trace := range p.ctx.Path {
		if trace.Flag == p.k.helpFlag {
			return true
		}
	}
	return false
}

// Values returns every grammar value together with the final value the plan will write.
func (p *Plan) Values() []PlannedValue { return append([]PlannedValue(nil), p.values...) }

// Flags returns the flags effective along the selected command path.
func (p *Plan) Flags() []PlannedFlag { return append([]PlannedFlag(nil), p.flags...) }

// Cancel discards the plan. A cancelled plan can no longer be committed.
func (p *Plan) Cancel() error {
	if p.state == PlanStateCommitted {
		return ErrPlanCommitted
	}
	p.state = PlanStateCancelled
	p.baselineRoot = reflect.Value{}
	p.resetRoot = reflect.Value{}
	p.externals = nil
	p.values = nil
	p.flags = nil
	return nil
}

// Commit applies the plan using the [Kong] instance that produced it.
//
// It is equivalent to p.Context().Kong.Commit(p).
func (p *Plan) Commit() (*Context, error) {
	return p.k.Commit(p)
}

// snapshotResetState captures the post-reset shadow state (root and external targets)
// together with the address maps needed to rebuild the same state at commit.
func (p *Plan) snapshotResetState(shadowRoot reflect.Value, externals *externalAllocator) {
	p.resetRoot, p.mShadowReset = deepClone(shadowRoot)
	p.mOrigShadowExt = append(addrMap(nil), externals.ranges...)
	for _, ext := range externals.targets {
		reset, m := deepClone(ext.shadow)
		ext.reset = reset
		ext.mShadowReset = m
		p.mShadowResetEx = append(p.mShadowResetEx, m...)
	}
}

// Preview performs the first, non-mutating phase of a two-phase parse.
//
// Preview runs the same internal pipeline as [Kong.Parse] (trace, reset, resolvers,
// apply and validation) against an isolated shadow copy of the grammar target. The
// original target is never written to, and Run, BeforeApply and AfterApply hooks do
// not run. BeforeReset and BeforeResolve hooks run against the shadow target, because
// they participate in value resolution (for example by adding context resolvers); the
// built-in help action is deferred to commit.
//
// The returned plan is returned together with a [ParseError] when tracing or validation
// fails, mirroring [Kong.Parse]; the plan is still returned (and inspectable) whenever
// tracing succeeded.
//
//nolint:gocyclo // the preview mirrors the staged pipeline of Parse step by step.
func (k *Kong) Preview(args []string) (plan *Plan, err error) {
	args = append([]string(nil), args...)
	metaBefore := snapshotModelMeta(k)

	root := k.Model.Target
	baselineRoot, _ := deepClone(root)
	shadowRoot, mOrigShadow := deepClone(root)

	plan = &Plan{
		k:               k,
		args:            args,
		grammar:         grammarFingerprint(k),
		baselineRoot:    baselineRoot,
		mOrigShadow:     mOrigShadow,
		externals:       map[uintptr]*externalTarget{},
		resolverResults: map[*Flag]resolverObservation{},
	}

	externals := newExternalAllocator()
	plan.externals = externals.targets
	translate := func(v reflect.Value) (reflect.Value, bool) {
		if shadow, ok := mOrigShadow.translate(v); ok {
			return shadow, true
		}
		return externals.ranges.translate(v)
	}
	restoreTargets := k.swapModelTargets(translate, externals.miss)

	fail := func(diag error, exitCode int) (*Plan, error) {
		restoreTargets()
		restoreModelMeta(k, metaBefore)
		plan.diag = diag
		plan.state = PlanStateError
		return plan, &ParseError{error: diag, Context: plan.ctx, exitCode: exitCode}
	}

	ctx, traceErr := Trace(k, args)
	plan.ctx = ctx
	if traceErr != nil {
		return fail(traceErr, exitUsageError)
	}
	if ctx.Error != nil {
		return fail(ctx.Error, exitUsageError)
	}

	ctx.resolveObserver = plan.observeResolver
	ctx.hookFilter = func(trace *Path, hook string) bool {
		return hook == "BeforeReset" && trace.Flag == k.helpFlag
	}

	// --help exits from the help flag's BeforeReset hook in Parse, before any reset,
	// resolution, apply or validation. Run the remaining BeforeReset hooks once, then
	// stop at the same boundary.
	if plan.HelpRequested() {
		if e := k.applyHook(ctx, "BeforeReset"); e != nil {
			plan.diag = e
			plan.state = PlanStateError
			restoreTargets()
			restoreModelMeta(k, metaBefore)
			return plan, &ParseError{error: e, Context: ctx}
		}
		plan.help = true
		plan.env = snapshotEnvironment(k)
		plan.snapshotResetState(shadowRoot, externals)
		plan.postMeta = snapshotModelMeta(k)
		plan.collectValues()
		restoreTargets()
		restoreModelMeta(k, metaBefore)
		ctx.hookFilter = nil
		plan.state = PlanStateReady
		return plan, nil
	}

	run := func(fn func() error) error {
		if e := fn(); e != nil {
			plan.diag = e
			plan.state = PlanStateError
			plan.env = snapshotEnvironment(k)
			restoreTargets()
			restoreModelMeta(k, metaBefore)
			return &ParseError{error: e, Context: ctx}
		}
		return nil
	}

	if e := run(func() error { return k.applyHook(ctx, "BeforeReset") }); e != nil {
		return plan, e
	}
	if e := run(ctx.Reset); e != nil {
		return plan, e
	}
	if e := run(func() error { return k.applyHook(ctx, "BeforeResolve") }); e != nil {
		return plan, e
	}
	if e := run(ctx.Resolve); e != nil {
		return plan, e
	}

	ctx.resolveObserver = nil
	plan.contextResolver = append([]Resolver(nil), ctx.resolvers...)
	plan.env = snapshotEnvironment(k)

	// Snapshot the post-reset/pre-apply state used to stage a commit.
	plan.snapshotResetState(shadowRoot, externals)

	if _, e := ctx.Apply(); e != nil {
		plan.diag = e
		plan.state = PlanStateError
		restoreTargets()
		restoreModelMeta(k, metaBefore)
		return plan, &ParseError{error: e, Context: ctx}
	}

	validateErr := ctx.Validate()

	postMeta := snapshotModelMeta(k)
	plan.collectValues()

	restoreTargets()
	restoreModelMeta(k, metaBefore)
	ctx.hookFilter = nil
	plan.postMeta = postMeta

	if validateErr != nil {
		plan.diag = validateErr
		plan.state = PlanStateInvalid
		return plan, &ParseError{error: validateErr, Context: ctx, exitCode: exitUsageError}
	}
	plan.state = PlanStateReady
	return plan, nil
}

func (p *Plan) observeResolver(flag *Flag, selected any) {
	p.resolverResults[flag] = resolverObservation{queried: true, selected: selected}
}

// collectValues builds the inspectable planned value/flag lists while the model
// targets are still swapped to the shadow graph, so reading a target yields exactly
// the final state the plan will apply.
func (p *Plan) collectValues() {
	sources := map[*Value]ValueSource{}
	for _, trace := range p.ctx.Path {
		switch {
		case trace.Flag != nil:
			sources[trace.Flag.Value] = SourceCLI
			if trace.Resolved {
				sources[trace.Flag.Value] = SourceResolver
			}
		case trace.Positional != nil:
			sources[trace.Positional] = SourceCLI
		case trace.Argument != nil:
			sources[trace.Argument.Argument] = SourceCLI
		}
	}

	sourceFor := func(value *Value) ValueSource {
		if source, ok := sources[value]; ok {
			return source
		}
		if len(value.Tag.Envs) != 0 && atLeastOneEnvSet(value.Tag.Envs) {
			return SourceEnv
		}
		if value.HasDefault {
			return SourceDefault
		}
		return SourceZero
	}

	_ = Visit(p.k.Model, func(node Visitable, next Next) error {
		if value, ok := node.(*Value); ok {
			p.values = append(p.values, PlannedValue{
				Value:  value,
				Source: sourceFor(value),
				final:  standaloneClone(value.Target),
			})
		}
		return next(nil)
	})

	seen := map[*Flag]bool{}
	for _, flag := range p.ctx.Flags() {
		if seen[flag] {
			continue
		}
		seen[flag] = true
		p.flags = append(p.flags, PlannedFlag{
			Flag:   flag,
			Source: sourceFor(flag.Value),
			value:  standaloneClone(flag.Target),
		})
	}
}

// Commit applies a validated plan to the grammar target.
//
// Commit verifies that the plan belongs to this Kong instance and that the grammar,
// environment, resolver results and grammar target are unchanged. It then replays the
// BeforeApply hook and Apply/Validate on an isolated staging copy, copies the plan's
// final state into the target in a single write boundary, and runs AfterApply. Any
// failure before the boundary leaves the target untouched; a failure afterwards rolls
// the target back to its pre-preview state.
//
//nolint:gocyclo // the commit runs the full validation boundary before any write.
func (k *Kong) Commit(p *Plan) (ctx *Context, err error) {
	if p == nil {
		return nil, errors.New("kong: cannot commit a nil plan")
	}
	if p.k != k {
		return nil, ErrPlanForeignKong
	}
	switch p.state {
	case PlanStateReady:
		// Proceed to commit.
	case PlanStateCommitted:
		return nil, ErrPlanCommitted
	case PlanStateCancelled:
		return nil, ErrPlanCancelled
	case PlanStateError:
		return nil, fmt.Errorf("%w: the command-line could not be parsed", ErrPlanInvalid)
	case PlanStateInvalid:
		if p.diag != nil {
			return nil, fmt.Errorf("%w: %s", ErrPlanInvalid, p.diag.Error())
		}
		return nil, ErrPlanInvalid
	}

	currentFP := grammarFingerprint(k)
	if currentFP != p.grammar {
		return nil, ErrPlanGrammarChanged
	}
	if !environmentsEqual(p.env, snapshotEnvironment(k)) {
		return nil, ErrPlanEnvironmentChanged
	}
	if !reflect.DeepEqual(safeInterfaceMust(k.Model.Target), safeInterfaceMust(p.baselineRoot)) {
		return nil, ErrPlanTargetModified
	}
	for _, ext := range p.externals {
		if !reflect.DeepEqual(safeInterfaceMust(ext.orig), safeInterfaceMust(ext.baseline)) {
			return nil, fmt.Errorf("%w: external command target", ErrPlanTargetModified)
		}
	}

	// Re-trace against the current grammar to verify command selection is unchanged.
	verifyMeta := snapshotModelMeta(k)
	verifyCtx, verifyTraceErr := Trace(k, p.args)
	verifyErr := verifyTraceErr
	if verifyErr == nil {
		verifyErr = verifyCtx.Error
	}
	samePath := pathsEqual(structuralPath(verifyCtx.Path), structuralPath(p.ctx.Path))
	restoreModelMeta(k, verifyMeta)
	if verifyErr != nil {
		return nil, fmt.Errorf("%w: %s", ErrPlanGrammarChanged, verifyErr.Error())
	}
	if !samePath {
		return nil, ErrPlanGrammarChanged
	}

	ctx = p.ctx

	// Help executes from BeforeReset in Parse, before resolvers ever run, so it is
	// dispatched directly and leaves no values behind.
	if p.help {
		if e := k.runHelpAction(ctx); e != nil {
			return nil, e
		}
		p.state = PlanStateCommitted
		p.release()
		return ctx, nil
	}

	verifyCtx.resolvers = append([]Resolver(nil), p.contextResolver...)
	if e := p.verifyResolvers(verifyCtx); e != nil {
		return nil, e
	}

	metaBefore := snapshotModelMeta(k)

	// Build the staging graph by chaining original -> shadow -> reset -> fresh stage
	// for both the grammar root and every external target.
	stageRoot, mRootResetStage := deepClone(p.resetRoot)
	rootToStage := composeAddrMaps(composeAddrMaps(p.mOrigShadow, p.mShadowReset), mRootResetStage)
	extResetStage := addrMap{}
	stagedExternals := map[uintptr]reflect.Value{}
	for base, ext := range p.externals {
		stage, m := deepClone(ext.reset)
		extResetStage = append(extResetStage, m...)
		stagedExternals[base] = stage
	}
	extToStage := composeAddrMaps(composeAddrMaps(p.mOrigShadowExt, p.mShadowResetEx), extResetStage)
	combined := append(append(addrMap{}, rootToStage...), extToStage...)

	restoreStaging := k.swapModelTargets(combined.translate, standaloneClone)

	abort := func(e error) (*Context, error) {
		restoreStaging()
		restoreModelMeta(k, metaBefore)
		return nil, e
	}

	if e := k.applyHook(ctx, "BeforeApply"); e != nil {
		return abort(e)
	}
	if _, e := ctx.Apply(); e != nil {
		return abort(e)
	}
	if e := ctx.Validate(); e != nil {
		return abort(e)
	}

	// Re-point the model at the real targets, then perform the single write boundary:
	// the staged final state is copied into every real target.
	restoreStaging()
	shadowCopyInto(k.Model.Target, stageRoot)
	for base, ext := range p.externals {
		if stage := stagedExternals[base]; stage.IsValid() {
			shadowCopyInto(ext.orig, stage)
		}
	}

	if e := k.applyHook(ctx, "AfterApply"); e != nil {
		shadowCopyInto(k.Model.Target, p.baselineRoot)
		for _, ext := range p.externals {
			shadowCopyInto(ext.orig, ext.baseline)
		}
		restoreModelMeta(k, metaBefore)
		return nil, e
	}

	restoreModelMeta(k, p.postMeta)
	p.state = PlanStateCommitted
	p.release()
	return ctx, nil
}

func (p *Plan) release() {
	p.baselineRoot = reflect.Value{}
	p.resetRoot = reflect.Value{}
	p.mOrigShadow = nil
	p.mOrigShadowExt = nil
	p.mShadowReset = nil
	p.mShadowResetEx = nil
	p.externals = nil
	p.values = nil
	p.flags = nil
}

// verifyResolvers re-queries every resolver consulted by the plan and compares the raw
// results with the recorded ones. Resolver functions must therefore be deterministic
// between preview and commit.
func (p *Plan) verifyResolvers(ctx *Context) error {
	visited := map[*Flag]bool{}
	for _, path := range ctx.Path {
		for _, flag := range path.Flags {
			// Command-line values short-circuit resolver lookup in Resolve.
			if _, ok := ctx.values[flag.Value]; ok {
				continue
			}
			var selected any
			for _, resolver := range ctx.combineResolvers() {
				raw, e := resolver.Resolve(ctx, path, flag)
				if e != nil {
					return fmt.Errorf("%w: %s: %s", ErrPlanResolverChanged, flag.ShortSummary(), e.Error())
				}
				if raw != nil {
					selected = raw
				}
			}
			recorded, ok := p.resolverResults[flag]
			if !ok || !recorded.queried {
				return fmt.Errorf("%w: %s", ErrPlanResolverChanged, flag.ShortSummary())
			}
			if !reflect.DeepEqual(selected, recorded.selected) {
				return fmt.Errorf("%w: %s would now resolve to %v", ErrPlanResolverChanged, flag.ShortSummary(), selected)
			}
			visited[flag] = true
		}
	}
	for flag := range p.resolverResults {
		if !visited[flag] {
			return fmt.Errorf("%w: %s", ErrPlanResolverChanged, flag.ShortSummary())
		}
	}
	return nil
}

// runHelpAction performs the deferred built-in help behaviour, exactly where
// BeforeReset would perform it during a normal Parse: before any target write.
func (k *Kong) runHelpAction(ctx *Context) error {
	for _, trace := range ctx.Path {
		if trace.Flag != k.helpFlag {
			continue
		}
		binds := k.bindings.clone()
		binds.add(ctx, trace)
		binds.add(trace.Node().Vars().CloneWith(k.vars))
		binds.merge(ctx.bindings)
		for _, method := range getMethods(k.helpFlag.Value.Target, "BeforeReset") {
			if e := callFunction(method, binds); e != nil {
				return e
			}
		}
	}
	return nil
}

// structuralPath returns trace entries produced directly by command-line parsing,
// excluding paths inserted for resolver-supplied values. Resolver inputs are verified
// separately.
func structuralPath(path []*Path) []*Path {
	out := make([]*Path, 0, len(path))
	for _, p := range path {
		if p.Resolved {
			continue
		}
		out = append(out, p)
	}
	return out
}

func pathsEqual(a, b []*Path) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Resolved != y.Resolved {
			return false
		}
		switch {
		case x.App != nil || y.App != nil:
			if x.App == nil || y.App == nil || x.App != y.App {
				return false
			}
		case x.Command != nil || y.Command != nil:
			if x.Command == nil || y.Command == nil || x.Command != y.Command {
				return false
			}
		case x.Argument != nil || y.Argument != nil:
			if x.Argument == nil || y.Argument == nil || x.Argument != y.Argument {
				return false
			}
		case x.Positional != nil || y.Positional != nil:
			if x.Positional == nil || y.Positional == nil || x.Positional != y.Positional {
				return false
			}
		case x.Flag != nil || y.Flag != nil:
			if x.Flag == nil || y.Flag == nil || x.Flag != y.Flag {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func snapshotEnvironment(k *Kong) map[string]envState {
	out := map[string]envState{}
	_ = Visit(k.Model, func(node Visitable, next Next) error {
		if value, ok := node.(*Value); ok {
			for _, env := range value.Tag.Envs {
				if _, seen := out[env]; seen {
					continue
				}
				v, set := os.LookupEnv(env)
				out[env] = envState{set: set, value: v}
			}
		}
		return next(nil)
	})
	return out
}

func environmentsEqual(a, b map[string]envState) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va != vb {
			return false
		}
	}
	return true
}

// grammarFingerprint computes a deterministic checksum of the effective grammar:
// command tree, flags, tags, groups, mappers, default commands and dynamic commands.
func grammarFingerprint(k *Kong) uint64 {
	h := fnv.New64a()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	writeStrings := func(prefix string, values []string) {
		write(prefix)
		write(strings.Join(values, ","))
	}
	_ = Visit(k.Model, func(node Visitable, next Next) error {
		switch n := node.(type) {
		case *Node:
			write("node")
			write(fmt.Sprint(n.Type))
			write(n.Name)
			writeStrings("aliases", n.Aliases)
			write(fmt.Sprint(n.Passthrough))
			write(fmt.Sprint(n.Hidden))
			if n.DefaultCmd != nil {
				write("default")
				write(n.DefaultCmd.Name)
				write(n.DefaultCmd.Tag.Default)
			}
			if n.Group != nil {
				write(n.Group.Key)
			}
		case *Value:
			write("value")
			write(n.Name)
			write(n.Target.Type().String())
			write(fmt.Sprintf("%T", n.Mapper))
			write(fmt.Sprint(n.HasDefault, n.Default, n.Enum, n.Required, n.Position))
			write(fmt.Sprint(n.PassthroughMode, n.Tag.Type, n.Tag.Negatable))
			writeStrings("envs", n.Tag.Envs)
			writeStrings("xor", n.Tag.Xor)
			writeStrings("and", n.Tag.And)
			writeStrings("lastwins", n.Tag.LastWins)
			if n.Flag != nil {
				write("flag")
				write(string(n.Flag.Short))
				writeStrings("flagaliases", n.Flag.Aliases)
			}
		}
		return next(nil)
	})
	for _, dc := range k.dynamicCommands {
		write("dynamic")
		write(dc.name)
		write(fmt.Sprintf("%T", dc.cmd))
	}
	for _, resolver := range k.resolvers {
		write("resolver")
		write(fmt.Sprintf("%T", resolver))
	}
	return h.Sum64()
}

// --- model metadata snapshots -----------------------------------------------------

type modelMeta struct {
	nodes  map[*Node]bool
	values map[*Value]valueMeta
	flags  map[*Flag]bool
}

type valueMeta struct {
	set      bool
	active   bool
	required bool
}

func snapshotModelMeta(k *Kong) *modelMeta {
	meta := &modelMeta{
		nodes:  map[*Node]bool{},
		values: map[*Value]valueMeta{},
		flags:  map[*Flag]bool{},
	}
	_ = Visit(k.Model, func(node Visitable, next Next) error {
		switch n := node.(type) {
		case *Node:
			meta.nodes[n] = n.Active
		case *Value:
			meta.values[n] = valueMeta{set: n.Set, active: n.Active, required: n.Required}
			if n.Flag != nil {
				meta.flags[n.Flag] = n.Flag.Negated
			}
		}
		return next(nil)
	})
	return meta
}

func restoreModelMeta(k *Kong, meta *modelMeta) {
	_ = Visit(k.Model, func(node Visitable, next Next) error {
		switch n := node.(type) {
		case *Node:
			if active, ok := meta.nodes[n]; ok {
				n.Active = active
			}
		case *Value:
			if m, ok := meta.values[n]; ok {
				n.Set = m.set
				n.Active = m.active
				n.Required = m.required
			}
			if n.Flag != nil {
				if negated, ok := meta.flags[n.Flag]; ok {
					n.Flag.Negated = negated
				}
			}
		}
		return next(nil)
	})
}

// --- model target swapping --------------------------------------------------------

// swapModelTargets temporarily replaces every target held by the grammar model with
// the shadow value returned by translate. Targets unknown to translate are obtained
// through miss. The returned function restores the original targets.
func (k *Kong) swapModelTargets(
	translate func(v reflect.Value) (reflect.Value, bool),
	miss func(orig reflect.Value) reflect.Value,
) func() {
	undos := []func(){}
	_ = Visit(k.Model, func(node Visitable, next Next) error {
		swap := func(value reflect.Value, set func(reflect.Value)) {
			if !value.IsValid() {
				return
			}
			shadow, ok := translate(value)
			if !ok {
				shadow = miss(value)
			}
			undos = append(undos, func() { set(value) })
			set(shadow)
		}
		switch n := node.(type) {
		case *Application:
			swap(n.Node.Target, func(v reflect.Value) { n.Node.Target = v })
		case *Node:
			swap(n.Target, func(v reflect.Value) { n.Target = v })
		case *Value:
			swap(n.Target, func(v reflect.Value) { n.Target = v })
		}
		return next(nil)
	})
	return func() {
		for i := len(undos) - 1; i >= 0; i-- {
			undos[i]()
		}
	}
}

// --- deep copy engine -------------------------------------------------------------

// allocRange records an independent allocation: bytes [old, old+size) in the source
// graph correspond to the allocation at ptr in the destination graph.
type allocRange struct {
	old  uintptr
	ptr  unsafe.Pointer
	size uintptr
}

type addrMap []allocRange

func (m addrMap) translate(v reflect.Value) (reflect.Value, bool) {
	if !v.CanAddr() {
		return reflect.Value{}, false
	}
	addr := v.UnsafeAddr()
	for _, r := range m {
		if addr >= r.old && addr < r.old+r.size {
			//nolint:gosec // offset translation inside an independently cloned graph.
			return reflect.NewAt(v.Type(), unsafe.Pointer(uintptr(r.ptr)+(addr-r.old))).Elem(), true
		}
	}
	return reflect.Value{}, false
}

// composeAddrMaps chains address maps, where the new addresses of outer are the old
// addresses of inner.
func composeAddrMaps(outer, inner addrMap) addrMap {
	out := make(addrMap, 0, len(outer))
	for _, r := range outer {
		for _, s := range inner {
			if s.old == uintptr(r.ptr) && s.size == r.size {
				out = append(out, allocRange{old: r.old, ptr: s.ptr, size: r.size})
				break
			}
		}
	}
	return out
}

// standaloneClone returns a fully independent copy of v.
func standaloneClone(src reflect.Value) reflect.Value {
	dst := reflect.New(src.Type()).Elem()
	copyValue(dst, src, nil)
	return dst
}

// deepClone returns an independent copy plus the address map translating source
// addresses (including the root value itself) into the cloned graph.
func deepClone(src reflect.Value) (reflect.Value, addrMap) {
	dst := reflect.New(src.Type()).Elem()
	m := addrMap{{
		old: src.UnsafeAddr(),
		//nolint:gosec // root allocation range of the independent clone.
		ptr:  unsafe.Pointer(dst.UnsafeAddr()),
		size: src.Type().Size(),
	}}
	copyValue(dst, src, &m)
	return dst, m
}

// shadowCopyInto copies src into an existing addressable destination, allocating
// fresh pointers, slices and maps so the destination never aliases the source.
func shadowCopyInto(dst, src reflect.Value) {
	copyValue(dst, src, nil)
}

func copyValue(dst, src reflect.Value, m *addrMap) {
	src = cleanView(src)
	switch src.Kind() {
	case reflect.Ptr:
		if src.IsNil() {
			return
		}
		np := reflect.New(src.Type().Elem())
		if m != nil {
			//nolint:gosec // allocation range recorded for shadow address translation.
			*m = append(*m, allocRange{
				old:  src.Pointer(),
				ptr:  unsafe.Pointer(np.Elem().UnsafeAddr()),
				size: src.Type().Elem().Size(),
			})
		}
		forceAssign(dst, np)
		copyValue(np.Elem(), src.Elem(), m)

	case reflect.Interface:
		if src.IsNil() {
			return
		}
		elem := src.Elem()
		clone := reflect.New(elem.Type()).Elem()
		copyValue(clone, elem, m)
		forceAssign(dst, clone)

	case reflect.Slice:
		if src.IsNil() {
			return
		}
		ns := reflect.MakeSlice(src.Type(), src.Len(), src.Len())
		if m != nil && src.Len() > 0 {
			//nolint:gosec // allocation range recorded for shadow address translation.
			*m = append(*m, allocRange{
				old:  src.Pointer(),
				ptr:  unsafe.Pointer(ns.Pointer()),
				size: src.Type().Elem().Size() * uintptr(src.Len()),
			})
		}
		forceAssign(dst, ns)
		for i := 0; i < src.Len(); i++ {
			copyValue(ns.Index(i), src.Index(i), m)
		}

	case reflect.Map:
		if src.IsNil() {
			return
		}
		nm := reflect.MakeMapWithSize(src.Type(), src.Len())
		forceAssign(dst, nm)
		iter := src.MapRange()
		for iter.Next() {
			key := standaloneClone(iter.Key())
			val := standaloneClone(iter.Value())
			nm.SetMapIndex(key, val)
		}

	case reflect.Struct:
		for i := 0; i < src.NumField(); i++ {
			copyValue(forceField(dst.Field(i)), src.Field(i), m)
		}

	case reflect.Array:
		for i := 0; i < src.Len(); i++ {
			copyValue(dst.Index(i), src.Index(i), m)
		}

	default:
		forceAssign(dst, src)
	}
}

// forceField returns a settable view of an addressable struct field, including
// unexported ones.
func forceField(v reflect.Value) reflect.Value {
	if v.CanSet() {
		return v
	}
	//nolint:gosec // write into an independent shadow copy of the caller's struct.
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
}

func forceAssign(dst, src reflect.Value) {
	src = cleanView(src)
	if !dst.CanSet() {
		//nolint:gosec // write into an independent shadow copy of the caller's struct.
		dst = reflect.NewAt(dst.Type(), unsafe.Pointer(dst.UnsafeAddr())).Elem()
	}
	dst.Set(src)
}

// cleanView strips the read-only flag reflect attaches to values reached through
// unexported fields or stored inside interfaces, so the result can be used as the
// source of a Set.
func cleanView(v reflect.Value) reflect.Value {
	if v.CanAddr() {
		if v.CanInterface() {
			return v
		}
		//nolint:gosec // read through an unexported field of an independent clone.
		return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	}
	if v.CanInterface() {
		return reflect.ValueOf(v.Interface())
	}
	return v
}

// safeInterface returns the Go value even when v was reached through an unexported
// field; the boolean reports whether interfacing succeeded at all.
func safeInterface(v reflect.Value) (any, bool) {
	if v.CanInterface() {
		return v.Interface(), true
	}
	if !v.CanAddr() {
		return nil, false
	}
	//nolint:gosec // read through an unexported field of an independent clone.
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface(), true
}

func safeInterfaceMust(v reflect.Value) any {
	if v.CanInterface() {
		return v.Interface()
	}
	//nolint:gosec // read through an unexported field of an independent clone.
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface()
}
