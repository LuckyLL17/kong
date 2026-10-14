package kong

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// Runtime command registry.
//
// Long-running host programs can register, replace and unload plugin
// commands after the parser has been built. Every mutating operation is
// applied atomically in batches against an immutable copy of the command
// tree: validation runs over the whole batch first, and a new tree version
// is published only when every command in the batch is valid. Parsing,
// help rendering and command discovery pin the tree version that was
// current when they started, so in-flight work never observes a partially
// updated tree.

const (
	// keySep separates the owner and the command id in an identity key,
	// and path elements in the tree index. It is not a valid identifier
	// character on its own.
	keySep = "\x00"
)

// RegistryErrorCode identifies the kind of a structured [RegistryError].
type RegistryErrorCode string

// Structured error codes returned by [CommandRegistry] operations.
const (
	// InvalidBatchError is returned when the batch itself is malformed (eg. no owner).
	InvalidBatchError RegistryErrorCode = "invalid_batch"
	// InvalidCommandError is returned for a malformed command specification
	// (bad identity, name, target type or a failure to build the command subtree).
	InvalidCommandError RegistryErrorCode = "invalid_command"
	// InvalidTagError is returned when the extra Kong tag fragments of a
	// command cannot be parsed.
	InvalidTagError RegistryErrorCode = "invalid_tag"
	// InvalidGroupError is returned when a command references a group that
	// is not declared through ExplicitGroups.
	InvalidGroupError RegistryErrorCode = "invalid_group"
	// NameConflictError is returned when a command name collides with a
	// static command, a legacy dynamic command or another batch entry.
	NameConflictError RegistryErrorCode = "name_conflict"
	// AliasConflictError is returned when a command alias collides with a
	// sibling command name or alias.
	AliasConflictError RegistryErrorCode = "alias_conflict"
	// FlagConflictError is returned when a flag inside a registered subtree
	// shadows a flag declared on an ancestor command.
	FlagConflictError RegistryErrorCode = "flag_conflict"
	// UnknownParentError is returned when the requested mount path does not
	// exist and is not provided by another command in the same batch.
	UnknownParentError RegistryErrorCode = "unknown_parent"
	// MountCycleError is returned when a command would be mounted beneath itself.
	MountCycleError RegistryErrorCode = "mount_cycle"
	// DuplicateIdentityError is returned when the same stable identity is
	// submitted more than once in a single batch.
	DuplicateIdentityError RegistryErrorCode = "duplicate_identity"
	// DuplicateTargetError is returned when the same command target pointer
	// is mounted under more than one identity.
	DuplicateTargetError RegistryErrorCode = "duplicate_target"
	// ForeignOwnerError is returned when a batch tries to modify a command
	// owned by a different registrant.
	ForeignOwnerError RegistryErrorCode = "foreign_owner"
	// CommandNotFoundError is returned when an unload names an identity that
	// is not currently registered.
	CommandNotFoundError RegistryErrorCode = "command_not_found"
	// DefaultCommandConflictError is returned when more than one command
	// would be the default command of the same parent.
	DefaultCommandConflictError RegistryErrorCode = "default_command_conflict"
	// DependentRegistrationError is returned when a subtree cannot be
	// unloaded or replaced because another registrant mounted commands
	// inside it.
	DependentRegistrationError RegistryErrorCode = "dependent_registration"
	// CommandsInUseError is returned when an unload or replacement targets
	// a command that is still referenced by a pinned command-tree snapshot.
	CommandsInUseError RegistryErrorCode = "commands_in_use"
)

// RegistryError describes a single validation problem found while applying
// a [BatchChange]. Every field except Code and Message is optional and
// provides structured context.
type RegistryError struct {
	// Code identifies the kind of problem.
	Code RegistryErrorCode
	// Owner is the registrant namespace of the offending command, if known.
	Owner string
	// ID is the stable command identifier within Owner, if known.
	ID string
	// Path is the command mount path, if known.
	Path []string
	// Message is a human-readable description.
	Message string
	// Cause is the underlying error, if any (eg. a tag or build error).
	Cause error
	// Related lists the other command identities involved in the problem,
	// eg. the sibling that owns a conflicting name or the dependent
	// registrations blocking an unload.
	Related []CommandIdentity
	// ActivePins reports how many pinned snapshots still reference a
	// command for a CommandsInUseError.
	ActivePins int
}

func (e *RegistryError) Error() string {
	var b strings.Builder
	b.WriteString(e.Message)
	if len(e.Path) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(e.Path, " "))
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap returns the underlying cause.
func (e *RegistryError) Unwrap() error { return e.Cause }

// RegistryErrors aggregates all problems detected while validating a batch.
// The batch is never partially applied: all listed problems must be fixed.
type RegistryErrors []*RegistryError

func (errs RegistryErrors) Error() string {
	messages := make([]string, len(errs))
	for i, err := range errs {
		messages[i] = "  - " + err.Error()
	}
	return fmt.Sprintf("command registry rejected the batch with %d problem(s):\n%s", len(errs), strings.Join(messages, "\n"))
}

// Unwrap allows errors.Is/errors.As to inspect every aggregated problem.
func (errs RegistryErrors) Unwrap() []error {
	out := make([]error, len(errs))
	for i, err := range errs {
		out[i] = err
	}
	return out
}

// IsRegistryError reports whether err (or any error in its tree) is a
// [RegistryError] with the given code.
func IsRegistryError(err error, code RegistryErrorCode) bool {
	var single *RegistryError
	if errors.As(err, &single) && single.Code == code {
		return true
	}
	var multi RegistryErrors
	if errors.As(err, &multi) {
		for _, e := range multi {
			if e.Code == code {
				return true
			}
		}
	}
	return false
}

// CommandIdentity is the stable identity of a runtime-registered command.
//
// Owner is the registrant namespace (eg. a plugin id) and ID uniquely
// identifies the command within that namespace. The pair survives command
// replacements, renames and remounts, and is what unload targets.
type CommandIdentity struct {
	Owner string
	ID    string
}

func (id CommandIdentity) key() string { return id.Owner + keySep + id.ID }

func (id CommandIdentity) valid() bool {
	return runtimeIdentRegexp.MatchString(id.Owner) && runtimeIdentRegexp.MatchString(id.ID)
}

func (id CommandIdentity) String() string { return id.Owner + "/" + id.ID }

// PluginCommand describes a single command in a runtime registration batch.
type PluginCommand struct {
	// ID is the stable identifier of the command within Owner. Required.
	ID string
	// Name is the command word on the command line. Defaults to ID when empty.
	Name string
	// Help is the short help string shown in command listings.
	Help string
	// Group is an optional help group key, resolved through ExplicitGroups.
	Group string
	// Parent is the mount path of the parent command, eg. []string{"db"}.
	// Nil or empty mounts the command at the application root.
	Parent []string
	// Tags holds extra Kong tag fragments, in the form key:"value", exactly
	// like the tags argument to DynamicCommand.
	Tags []string
	// Cmd must be a pointer to a command structure (or a runnable func
	// value). Each parse receives a private copy of struct-backed commands.
	Cmd any
}

// BatchChange is an atomic set of runtime command changes.
//
// Commands in Register sharing an identity with an existing command replace
// it; other commands are added. Commands named in Remove are unloaded. All
// changes either take effect together or do not take effect at all.
type BatchChange struct {
	// Owner identifies the registrant submitting the batch. It may only
	// modify commands previously registered under the same owner.
	Owner string
	// Register lists commands to add or replace.
	Register []PluginCommand
	// Remove lists IDs of commands to unload.
	Remove []string
	// Force unloads commands even when they are referenced by pinned
	// command-tree snapshots. Existing snapshots keep working against
	// their pinned tree; the commands simply disappear from new parses.
	Force bool
}

// ReplacedCommand describes a command replaced by a batch.
type ReplacedCommand struct {
	// Identity is the stable identity of the replaced command.
	Identity CommandIdentity
	// PreviousGeneration is the tree version the replaced definition came from.
	PreviousGeneration uint64
}

// BatchResult reports what an applied [BatchChange] did.
type BatchResult struct {
	// Generation is the command-tree version produced by the batch, or the
	// current generation when the batch was an idempotent no-op.
	Generation uint64
	// Added lists identities mounted for the first time.
	Added []CommandIdentity
	// Replaced lists identities whose command node was replaced.
	Replaced []ReplacedCommand
	// Removed lists identities that were unloaded.
	Removed []CommandIdentity
	// Unchanged lists identities that were resubmitted with an identical
	// definition and therefore required no new tree version.
	Unchanged []CommandIdentity
}

// RegisteredCommand describes a currently registered command.
type RegisteredCommand struct {
	// Identity is the stable identity of the command.
	Identity CommandIdentity
	// Name is the command word on the command line.
	Name string
	// Path is the full command mount path, including Name.
	Path []string
	// Aliases are the command aliases.
	Aliases []string
	// Group is the help group key, if any.
	Group string
	// Generation is the tree version in which the current definition was installed.
	Generation uint64
}

var runtimeIdentRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// registration is the registry-level record of one stable command identity.
type registration struct {
	identity   CommandIdentity
	spec       PluginCommand
	name       string
	parent     []string
	proto      reflect.Value // prepared prototype target (struct command) or shared leaf target
	leafFunc   bool
	targetPtr  uintptr
	generation uint64
}

func (r *registration) path() []string {
	out := make([]string, 0, len(r.parent)+1)
	out = append(out, r.parent...)
	out = append(out, r.name)
	return out
}

// freshInstance returns a private target value for one parse. Struct
// commands are deep-copied so concurrent parses never share mutable state;
// leaf commands (runnable func values) are shared read-only.
func (r *registration) freshInstance() reflect.Value {
	if r.leafFunc {
		return r.proto
	}
	return deepCopyValue(r.proto)
}

// treeVersion is one immutable, published command tree.
type treeVersion struct {
	id uint64
	// app is the immutable published model.
	app *Application
	// commands indexes command nodes by full path key.
	commands map[string]*Node
	// records maps runtime command subtree roots in this tree to their
	// registration. Used to allocate per-parse target copies.
	records map[*Node]*registration
	// ids indexes registrations by identity key.
	ids map[string]*registration
	// pins counts parse/help snapshots currently referencing this version.
	pins int
}

func (v *treeVersion) rebuild() {
	v.commands = map[string]*Node{}
	v.ids = map[string]*registration{}
	for _, reg := range v.records {
		v.ids[reg.identity.key()] = reg
	}
	var walk func(n *Node, parts []string)
	walk = func(n *Node, parts []string) {
		if n.Type == CommandNode {
			v.commands[strings.Join(parts, keySep)] = n
		}
		for _, child := range n.Children {
			walk(child, append(parts, child.Name))
		}
	}
	walk(v.app.Node, nil)
}

// versionPin keeps a tree version alive for in-flight parsing.
type versionPin struct {
	r *CommandRegistry
	v *treeVersion
}

func (p *versionPin) release() {
	if p == nil {
		return
	}
	p.r.release(p.v)
}

// CommandRegistry allows long-running host programs to register, replace
// and unload plugin commands at runtime.
//
// Obtain one with [Kong.CommandRegistry] after enabling it through the
// RuntimeCommands() option.
type CommandRegistry struct {
	k *Kong

	mu       sync.Mutex
	current  atomic.Pointer[treeVersion]
	versions map[uint64]*treeVersion // current version plus versions with pins
	records  map[string]*registration
	nextID   uint64
}

func newCommandRegistry(k *Kong) *CommandRegistry {
	return &CommandRegistry{
		k:        k,
		versions: map[uint64]*treeVersion{},
		records:  map[string]*registration{},
		nextID:   1,
	}
}

func (r *CommandRegistry) initialize() {
	v := &treeVersion{
		id:      r.nextID,
		app:     r.k.Model,
		records: map[*Node]*registration{},
	}
	v.rebuild()
	r.versions[v.id] = v
	r.current.Store(v)
	r.nextID++
}

// Current returns the most recently published command tree. The returned
// application is immutable and safe to traverse concurrently with ongoing
// registrations and parses.
func (r *CommandRegistry) Current() *Application {
	return r.current.Load().app
}

// Generation returns the current command-tree version number.
func (r *CommandRegistry) Generation() uint64 {
	return r.current.Load().id
}

// Registered returns the currently registered plugin commands, ordered by
// owner and id. Commands registered through DynamicCommand are not included.
func (r *CommandRegistry) Registered() []RegisteredCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.current.Load()
	out := make([]RegisteredCommand, 0, len(r.records))
	for _, reg := range r.records {
		entry := RegisteredCommand{
			Identity:   reg.identity,
			Name:       reg.name,
			Path:       reg.path(),
			Group:      reg.spec.Group,
			Generation: reg.generation,
		}
		if node := current.commands[joinPath(entry.Path)]; node != nil {
			entry.Aliases = append([]string(nil), node.Aliases...)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Identity.Owner != out[j].Identity.Owner {
			return out[i].Identity.Owner < out[j].Identity.Owner
		}
		return out[i].Identity.ID < out[j].Identity.ID
	})
	return out
}

// Find returns the registered command with the given identity.
func (r *CommandRegistry) Find(owner, id string) (RegisteredCommand, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.records[CommandIdentity{Owner: owner, ID: id}.key()]
	if !ok {
		return RegisteredCommand{}, false
	}
	current := r.current.Load()
	path := reg.path()
	entry := RegisteredCommand{
		Identity:   reg.identity,
		Name:       reg.name,
		Path:       path,
		Group:      reg.spec.Group,
		Generation: reg.generation,
	}
	if node := current.commands[joinPath(path)]; node != nil {
		entry.Aliases = append([]string(nil), node.Aliases...)
	}
	return entry, true
}

// findRegistrationByID scans all owners for a command with the given id.
// It is used to attribute a foreign-owner unload attempt.
func (r *CommandRegistry) findRegistrationByID(id string) *registration {
	for _, reg := range r.records {
		if reg.identity.ID == id {
			return reg
		}
	}
	return nil
}

// acquire pins the current tree version for a new parse or help render. The
// caller must call release on the returned pin exactly once.
func (r *CommandRegistry) acquire() *versionPin {
	v := r.current.Load()
	r.mu.Lock()
	v.pins++
	r.versions[v.id] = v
	r.mu.Unlock()
	return &versionPin{r: r, v: v}
}

func (r *CommandRegistry) release(v *treeVersion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v.pins > 0 {
		v.pins--
	}
	if v.pins == 0 && v.id != r.current.Load().id {
		delete(r.versions, v.id)
	}
}

// cloneForTrace produces the private model copy a parse works against.
// Static and legacy nodes keep pointing at the grammar targets; runtime
// command subtrees get freshly allocated, deep-copied targets.
func (r *CommandRegistry) cloneForTrace(v *treeVersion) *Application {
	cloner := newTreeCloner()
	app := cloner.cloneApplication(v.app)
	for srcNode, reg := range v.records {
		cp := cloner.nodes[srcNode]
		if cp == nil {
			continue
		}
		rebindDynamicNode(r.k, reg.identity.key(), srcNode, cp, reg.freshInstance())
	}
	return app
}

// Register is a convenience for applying a batch that only adds or replaces
// commands owned by owner.
func (r *CommandRegistry) Register(owner string, commands ...PluginCommand) (*BatchResult, error) {
	return r.Apply(&BatchChange{Owner: owner, Register: commands})
}

// Unregister is a convenience for applying a batch that removes commands
// owned by owner.
func (r *CommandRegistry) Unregister(owner string, ids ...string) (*BatchResult, error) {
	return r.Apply(&BatchChange{Owner: owner, Remove: ids})
}

// plannedCommand is a validated, offline-built command awaiting commit.
type plannedCommand struct {
	spec      PluginCommand
	identity  CommandIdentity
	name      string
	parent    []string
	node      *Node
	proto     reflect.Value
	isDefault bool
	leafFunc  bool
	targetPtr uintptr
	replaces  *registration
	unchanged bool
}

func (p *plannedCommand) mountPath() []string {
	out := make([]string, 0, len(p.parent)+1)
	out = append(out, p.parent...)
	out = append(out, p.name)
	return out
}

func (p *plannedCommand) toRegistration(generation uint64) *registration {
	return &registration{
		identity:   p.identity,
		spec:       p.spec,
		name:       p.name,
		parent:     append([]string(nil), p.parent...),
		proto:      p.proto,
		leafFunc:   p.leafFunc,
		targetPtr:  p.targetPtr,
		generation: generation,
	}
}

// Apply validates and atomically applies a batch of command changes.
func (r *CommandRegistry) Apply(change *BatchChange) (*BatchResult, error) {
	if change == nil {
		change = &BatchChange{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	current := r.current.Load()
	errs := []*RegistryError{}

	owner := strings.TrimSpace(change.Owner)
	if owner == "" {
		errs = append(errs, &RegistryError{
			Code:    InvalidBatchError,
			Message: "batch requires a non-empty owner",
		})
	} else if !runtimeIdentRegexp.MatchString(owner) {
		errs = append(errs, &RegistryError{
			Code:    InvalidBatchError,
			Owner:   owner,
			Message: fmt.Sprintf("owner %q is not a valid namespace (allowed: letters, digits, '.', '_', '-')", owner),
		})
	}

	removeKeys := map[string]*registration{}
	for _, rawID := range change.Remove {
		id := strings.TrimSpace(rawID)
		ident := CommandIdentity{Owner: owner, ID: id}
		if !runtimeIdentRegexp.MatchString(id) {
			errs = append(errs, &RegistryError{
				Code:    CommandNotFoundError,
				Owner:   owner,
				ID:      id,
				Message: fmt.Sprintf("cannot unload %q: not a valid command id", id),
			})
			continue
		}
		reg, ok := r.records[ident.key()]
		if !ok {
			if foreign := r.findRegistrationByID(id); foreign != nil {
				errs = append(errs, &RegistryError{
					Code:    ForeignOwnerError,
					Owner:   owner,
					ID:      id,
					Path:    foreign.path(),
					Related: []CommandIdentity{foreign.identity},
					Message: fmt.Sprintf("cannot unload %s: %s is owned by %q", ident, foreign.identity, foreign.identity.Owner),
				})
			} else {
				errs = append(errs, &RegistryError{
					Code:    CommandNotFoundError,
					Owner:   owner,
					ID:      id,
					Message: fmt.Sprintf("cannot unload %s: command is not registered", ident),
				})
			}
			continue
		}
		removeKeys[ident.key()] = reg
	}

	plans := make([]*plannedCommand, 0, len(change.Register))
	seenIdentities := map[string]bool{}
	seenTargets := map[uintptr]CommandIdentity{}
	for _, reg := range r.records {
		seenTargets[reg.targetPtr] = reg.identity
	}

	for i := range change.Register {
		spec := change.Register[i]
		plan, specErrs := r.planCommand(owner, spec, seenIdentities, seenTargets)
		errs = append(errs, specErrs...)
		if plan != nil {
			plans = append(plans, plan)
			seenIdentities[plan.identity.key()] = true
			seenTargets[plan.targetPtr] = plan.identity
		}
	}

	// Run every validation phase even if an earlier one found problems, so
	// all issues in a batch are reported in one error. Plans whose subtree
	// failed to build are skipped by the structural phases.
	errs = append(errs, r.resolveParents(owner, current, plans)...)
	errs = append(errs, r.validateMountPoints(owner, current, plans)...)

	// Dependent registrations and in-use snapshots block removal and replacement.
	for _, key := range sortedRegistrationKeys(removeKeys) {
		errs = append(errs, r.validateRemoval(current, removeKeys[key], change.Force)...)
	}
	for _, plan := range plans {
		if plan.replaces != nil && !plan.unchanged {
			errs = append(errs, r.validateRemoval(current, plan.replaces, change.Force)...)
		}
	}

	if len(errs) > 0 {
		return nil, dedupeRegistryErrors(errs)
	}

	mutated := len(removeKeys) > 0
	for _, plan := range plans {
		if !plan.unchanged {
			mutated = true
		}
	}
	if !mutated {
		// Every entry is an identical resubmission: idempotent no-op on the
		// current generation.
		result := &BatchResult{Generation: current.id}
		for _, plan := range plans {
			if plan.unchanged {
				result.Unchanged = append(result.Unchanged, plan.identity)
			}
		}
		sortIdentities(result.Unchanged)
		return result, nil
	}

	return r.commit(current, change, plans, removeKeys)
}

func (r *CommandRegistry) planCommand(owner string, spec PluginCommand, seenIdentities map[string]bool, seenTargets map[uintptr]CommandIdentity) (*plannedCommand, []*RegistryError) {
	errs := []*RegistryError{}
	ident := CommandIdentity{Owner: owner, ID: strings.TrimSpace(spec.ID)}
	plan := &plannedCommand{spec: spec, identity: ident}

	if !runtimeIdentRegexp.MatchString(ident.ID) {
		errs = append(errs, &RegistryError{
			Code:    InvalidCommandError,
			Owner:   owner,
			ID:      spec.ID,
			Message: fmt.Sprintf("command id %q is not valid (allowed: letters, digits, '.', '_', '-')", spec.ID),
		})
	} else if seenIdentities[ident.key()] {
		errs = append(errs, &RegistryError{
			Code:    DuplicateIdentityError,
			Owner:   owner,
			ID:      ident.ID,
			Message: fmt.Sprintf("command %s is submitted more than once in the same batch", ident),
		})
	}

	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = ident.ID
	}
	plan.name = name
	if !runtimeIdentRegexp.MatchString(name) {
		errs = append(errs, &RegistryError{
			Code:    InvalidCommandError,
			Owner:   owner,
			ID:      ident.ID,
			Message: fmt.Sprintf("command %s has invalid command name %q", ident, name),
		})
	}
	parent := append([]string(nil), spec.Parent...)
	for _, part := range parent {
		if strings.TrimSpace(part) == "" {
			errs = append(errs, &RegistryError{
				Code:    InvalidCommandError,
				Owner:   owner,
				ID:      ident.ID,
				Path:    parent,
				Message: fmt.Sprintf("command %s has an empty path element in its parent mount path", ident),
			})
		}
	}
	plan.parent = parent

	if spec.Group != "" {
		switch {
		case !runtimeIdentRegexp.MatchString(spec.Group):
			errs = append(errs, &RegistryError{
				Code:    InvalidGroupError,
				Owner:   owner,
				ID:      ident.ID,
				Path:    plan.mountPath(),
				Message: fmt.Sprintf("command %s references invalid group %q", ident, spec.Group),
			})
		case len(r.k.groups) > 0 && !groupDeclared(r.k, spec.Group):
			errs = append(errs, &RegistryError{
				Code:    InvalidGroupError,
				Owner:   owner,
				ID:      ident.ID,
				Path:    plan.mountPath(),
				Message: fmt.Sprintf("command %s references undeclared group %q", ident, spec.Group),
			})
		}
	}

	rv := reflect.ValueOf(spec.Cmd)
	if !rv.IsValid() || rv.Kind() != reflect.Ptr || rv.IsNil() {
		errs = append(errs, &RegistryError{
			Code:    InvalidCommandError,
			Owner:   owner,
			ID:      ident.ID,
			Path:    plan.mountPath(),
			Message: fmt.Sprintf("command %s requires a non-nil pointer target", ident),
		})
		return plan, errs
	}
	plan.targetPtr = rv.Pointer()
	if other, ok := seenTargets[plan.targetPtr]; ok && other != ident {
		errs = append(errs, &RegistryError{
			Code:    DuplicateTargetError,
			Owner:   owner,
			ID:      ident.ID,
			Path:    plan.mountPath(),
			Related: []CommandIdentity{other},
			Message: fmt.Sprintf("command %s shares its target with %s; each command identity needs its own target", ident, other),
		})
	}

	if existing, ok := r.records[ident.key()]; ok {
		plan.replaces = existing
		if specsEqual(existing, spec, name, parent) {
			plan.unchanged = true
		}
	}

	if len(errs) > 0 {
		return plan, errs
	}
	if plan.unchanged {
		plan.node = nil
		return plan, nil
	}

	node, proto, leaf, buildErrs := r.buildPluginNode(plan)
	errs = append(errs, buildErrs...)
	if len(buildErrs) == 0 {
		plan.node = node
		plan.proto = proto
		plan.leafFunc = leaf
		plan.isDefault = node.Tag.HasDefault
	}
	return plan, errs
}

// buildPluginNode builds the command subtree offline against a private
// prototype value, exactly mirroring how New() synthesises DynamicCommand
// nodes, and validating tags, flags and grammar in the process.
func (r *CommandRegistry) buildPluginNode(plan *plannedCommand) (*Node, reflect.Value, bool, []*RegistryError) {
	spec := plan.spec
	tag, err := parseTagString(strings.Join(spec.Tags, " "))
	if err != nil {
		return nil, reflect.Value{}, false, []*RegistryError{{
			Code:    InvalidTagError,
			Owner:   plan.identity.Owner,
			ID:      plan.identity.ID,
			Path:    plan.mountPath(),
			Cause:   err,
			Message: fmt.Sprintf("command %s has invalid tags", plan.identity),
		}}
	}
	tag.Name = plan.name
	tag.Help = spec.Help
	tag.Group = spec.Group
	tag.Cmd = true

	userTarget := reflect.Indirect(reflect.ValueOf(spec.Cmd))
	proto := reflect.New(userTarget.Type()).Elem()
	proto.Set(userTarget)
	leaf := userTarget.Kind() != reflect.Struct

	scratch := &Node{Type: CommandNode, Tag: newEmptyTag()}
	seenFlags := map[string]bool{}
	mountParent := r.k.Model.Node
	if len(plan.parent) > 0 {
		if parent := r.current.Load().commands[joinPath(plan.parent)]; parent != nil {
			mountParent = parent
		}
	}
	seedAncestorFlags(mountParent, seenFlags)

	field := reflect.StructField{Name: plan.name, Type: userTarget.Type()}
	if err := buildChild(r.k, scratch, CommandNode, proto, field, proto, tag, plan.name, seenFlags); err != nil {
		return nil, proto, leaf, []*RegistryError{{
			Code:    InvalidCommandError,
			Owner:   plan.identity.Owner,
			ID:      plan.identity.ID,
			Path:    plan.mountPath(),
			Cause:   err,
			Message: fmt.Sprintf("command %s is invalid", plan.identity),
		}}
	}
	node := scratch.Children[len(scratch.Children)-1]
	node.registryID = plan.identity.key()
	for _, alias := range node.Aliases {
		if alias == node.Name || !runtimeIdentRegexp.MatchString(alias) {
			return nil, proto, leaf, []*RegistryError{{
				Code:    AliasConflictError,
				Owner:   plan.identity.Owner,
				ID:      plan.identity.ID,
				Path:    plan.mountPath(),
				Message: fmt.Sprintf("command %s has invalid or self-aliasing alias %q", plan.identity, alias),
			}}
		}
	}
	return node, proto, leaf, nil
}

func seedAncestorFlags(node *Node, seen map[string]bool) {
	for _, group := range node.AllFlags(false) {
		for _, flag := range group {
			seen["--"+flag.Name] = true
			if flag.Short != 0 {
				seen["-"+string(flag.Short)] = true
			}
			if neg := negatableFlagName(flag.Name, flag.Tag.Negatable); neg != "" {
				seen[neg] = true
			}
			for _, alias := range flag.Aliases {
				if utf8.RuneCountInString(alias) == 1 {
					seen["-"+alias] = true
				} else {
					seen["--"+alias] = true
				}
			}
		}
	}
}

func groupDeclared(k *Kong, key string) bool {
	for _, group := range k.groups {
		if group.Key == key {
			return true
		}
	}
	return false
}

// resolveParents validates mount paths, allowing batch entries to provide
// each other's parents (cross-dependencies), rejects self-mount cycles and
// checks flag shadowing across batch-provided parent chains.
func (r *CommandRegistry) resolveParents(owner string, current *treeVersion, plans []*plannedCommand) []*RegistryError {
	errs := []*RegistryError{}
	provided := map[string]bool{"": true}
	byPath := map[string]*plannedCommand{}
	for _, plan := range plans {
		provided[joinPath(plan.mountPath())] = true
		byPath[joinPath(plan.mountPath())] = plan
	}
	for _, plan := range plans {
		if plan.unchanged {
			continue
		}
		parentKey := joinPath(plan.parent)
		ownKey := joinPath(plan.mountPath())
		if parentKey == "" {
			continue
		}
		if _, exists := current.commands[parentKey]; !exists && !provided[parentKey] {
			errs = append(errs, &RegistryError{
				Code:    UnknownParentError,
				Owner:   owner,
				ID:      plan.identity.ID,
				Path:    append([]string(nil), plan.parent...),
				Message: fmt.Sprintf("command %s cannot be mounted at %s: parent command does not exist", plan.identity, strings.Join(plan.mountPath(), " ")),
			})
			continue
		}
		if parentKey == ownKey || strings.HasPrefix(parentKey, ownKey+keySep) {
			errs = append(errs, &RegistryError{
				Code:    MountCycleError,
				Owner:   owner,
				ID:      plan.identity.ID,
				Path:    plan.mountPath(),
				Message: fmt.Sprintf("command %s cannot be mounted beneath itself", plan.identity),
			})
			continue
		}
		// The parent exists in the current tree and is this command's own
		// current node: moving it beneath its own subtree would create a cycle.
		if parentNode, ok := current.commands[parentKey]; ok && parentNode.registryID == plan.identity.key() {
			errs = append(errs, &RegistryError{
				Code:    MountCycleError,
				Owner:   owner,
				ID:      plan.identity.ID,
				Path:    plan.mountPath(),
				Message: fmt.Sprintf("command %s cannot be mounted beneath its own subtree", plan.identity),
			})
		}
		// When the parent is provided by the same batch its flags were not
		// seeded at offline build time; validate inheritance explicitly.
		if plan.node != nil {
			if _, inCurrent := current.commands[parentKey]; !inCurrent {
				if provider := byPath[parentKey]; provider != nil && provider.node != nil {
					errs = append(errs, flagConflictsWithAncestors(owner, plan, provider.node)...)
				}
			}
		}
	}
	return errs
}

// flagConflictsWithAncestors reports every flag in plan's subtree that
// duplicates an inherited ancestor flag.
func flagConflictsWithAncestors(owner string, plan *plannedCommand, ancestor *Node) []*RegistryError {
	seen := map[string]bool{}
	seedAncestorFlags(ancestor, seen)
	var errs []*RegistryError
	_ = Visit(plan.node, func(v Visitable, next Next) error {
		flag, ok := v.(*Flag)
		if !ok {
			return next(nil)
		}
		if seen["--"+flag.Name] {
			errs = append(errs, &RegistryError{
				Code:    FlagConflictError,
				Owner:   owner,
				ID:      plan.identity.ID,
				Path:    plan.mountPath(),
				Message: fmt.Sprintf("command %s declares duplicate flag --%s inherited from its parent", plan.identity, flag.Name),
			})
		}
		return next(nil)
	})
	return errs
}

// wordOwner attributes a sibling command word to either a protected static
// or legacy node, or a runtime registration.
type wordOwner struct {
	node    *Node
	runtime *registration
}

func validateMountPointWords(current *treeVersion, disappearing map[*Node]bool, plans []*plannedCommand) []*RegistryError {
	errs := []*RegistryError{}
	type mountPoint struct {
		parentKey string
		words     map[string]wordOwner
		defaults  []string // descriptive labels
	}
	points := map[string]*mountPoint{}

	getPoint := func(parentKey string) *mountPoint {
		mp := points[parentKey]
		if mp != nil {
			return mp
		}
		mp = &mountPoint{parentKey: parentKey, words: map[string]wordOwner{}}
		var node *Node
		if parentKey == "" {
			node = current.app.Node
		} else {
			node = current.commands[parentKey]
		}
		if node != nil {
			for _, child := range node.Children {
				if child.Type != CommandNode {
					continue
				}
				var reg *registration
				if child.registryID != "" {
					reg = current.ids[child.registryID]
				}
				wo := wordOwner{node: child, runtime: reg}
				if !disappearing[child] {
					mp.words[child.Name] = wo
					for _, alias := range child.Aliases {
						mp.words[alias] = wo
					}
					if node.DefaultCmd == child {
						mp.defaults = append(mp.defaults, describeWordOwner(wo))
					}
				}
			}
		}
		points[parentKey] = mp
		return mp
	}

	for _, plan := range plans {
		if plan.unchanged {
			continue
		}
		getPoint(joinPath(plan.parent))
		if plan.replaces != nil {
			getPoint(joinPath(plan.replaces.parent))
		}
	}

	for _, plan := range plans {
		if plan.unchanged || plan.node == nil {
			continue
		}
		mp := getPoint(joinPath(plan.parent))
		path := plan.mountPath()
		claim := func(word string, isAlias bool) {
			existing, conflict := mp.words[word]
			if !conflict {
				return
			}
			code := NameConflictError
			if isAlias || existing.node.Name != word {
				code = AliasConflictError
			}
			label := "<root>"
			if len(plan.parent) > 0 {
				label = strings.Join(plan.parent, " ")
			}
			e := &RegistryError{
				Code:    code,
				Owner:   plan.identity.Owner,
				ID:      plan.identity.ID,
				Path:    path,
				Message: fmt.Sprintf("command %s cannot claim %q under %s: already used by %s", plan.identity, word, label, describeWordOwner(existing)),
			}
			if existing.runtime != nil {
				e.Related = []CommandIdentity{existing.runtime.identity}
			}
			errs = append(errs, e)
		}
		claim(plan.name, false)
		for _, alias := range plan.node.Aliases {
			claim(alias, true)
		}
		placeholder := wordOwner{node: plan.node, runtime: &registration{identity: plan.identity}}
		if _, exists := mp.words[plan.name]; !exists {
			mp.words[plan.name] = placeholder
		}
		for _, alias := range plan.node.Aliases {
			if _, exists := mp.words[alias]; !exists {
				mp.words[alias] = placeholder
			}
		}
		if plan.isDefault {
			mp.defaults = append(mp.defaults, plan.identity.String())
		}
	}

	keys := make([]string, 0, len(points))
	for key := range points {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		mp := points[key]
		seen := map[string]bool{}
		unique := []string{}
		for _, label := range mp.defaults {
			if !seen[label] {
				seen[label] = true
				unique = append(unique, label)
			}
		}
		if len(unique) > 1 {
			label := "<root>"
			if key != "" {
				label = strings.ReplaceAll(key, keySep, " ")
			}
			errs = append(errs, &RegistryError{
				Code:    DefaultCommandConflictError,
				Owner:   plans[0].identity.Owner,
				Path:    splitKey(key),
				Message: fmt.Sprintf("parent %s would have multiple default commands: %s", label, strings.Join(unique, ", ")),
			})
		}
	}
	return errs
}

// validateMountPoints runs all cross-command mount checks for one batch.
func (r *CommandRegistry) validateMountPoints(owner string, current *treeVersion, plans []*plannedCommand) []*RegistryError {
	disappearing := map[*Node]bool{}
	for _, plan := range plans {
		if plan.unchanged {
			continue
		}
		if plan.replaces != nil {
			if node := current.commands[joinPath(plan.replaces.path())]; node != nil {
				disappearing[node] = true
			}
		}
	}
	errs := validateMountPointWords(current, disappearing, plans)
	for _, e := range errs {
		if e.Owner == "" {
			e.Owner = owner
		}
	}
	return errs
}

func describeWordOwner(wo wordOwner) string {
	if wo.runtime != nil {
		return wo.runtime.identity.String()
	}
	if wo.node.legacyDynamic {
		return "dynamic command " + wo.node.Name
	}
	return "static command " + wo.node.Name
}

func splitKey(key string) []string {
	if key == "" {
		return nil
	}
	return strings.Split(key, keySep)
}

func joinPath(parts []string) string {
	return strings.Join(parts, keySep)
}

// validateRemoval enforces ownership of nested registrations and refuses to
// retire commands still referenced by pinned snapshots unless forced.
func (r *CommandRegistry) validateRemoval(current *treeVersion, reg *registration, force bool) []*RegistryError {
	errs := []*RegistryError{}
	node := current.commands[joinPath(reg.path())]
	if node != nil {
		dependents := []CommandIdentity{}
		_ = Visit(node, func(v Visitable, next Next) error {
			n, ok := v.(*Node)
			if ok && n.registryID != "" && n.registryID != reg.identity.key() {
				if other := current.ids[n.registryID]; other != nil {
					dependents = append(dependents, other.identity)
				}
			}
			return next(nil)
		})
		if len(dependents) > 0 {
			sortIdentities(dependents)
			errs = append(errs, &RegistryError{
				Code:    DependentRegistrationError,
				Owner:   reg.identity.Owner,
				ID:      reg.identity.ID,
				Path:    reg.path(),
				Related: dependents,
				Message: fmt.Sprintf("cannot remove %s: %d other registration(s) are mounted inside it", reg.identity, len(dependents)),
			})
		}
	}

	pins := 0
	for _, version := range r.versions {
		if _, ok := version.ids[reg.identity.key()]; ok {
			pins += version.pins
		}
	}
	if pins > 0 && !force {
		errs = append(errs, &RegistryError{
			Code:       CommandsInUseError,
			Owner:      reg.identity.Owner,
			ID:         reg.identity.ID,
			Path:       reg.path(),
			ActivePins: pins,
			Message: fmt.Sprintf("cannot remove %s: still referenced by %d active parse/help snapshot(s); "+
				"wait for them to finish or set BatchChange.Force to true", reg.identity, pins),
		})
	}
	return errs
}

// commit applies a validated batch by cloning the current tree, splicing
// the changes and publishing a single new immutable version.
func (r *CommandRegistry) commit(current *treeVersion, change *BatchChange, plans []*plannedCommand, removeKeys map[string]*registration) (*BatchResult, error) {
	result := &BatchResult{Generation: current.id}
	if len(plans) == 0 && len(removeKeys) == 0 {
		return result, nil
	}

	cloner := newTreeCloner()
	app := cloner.cloneApplication(current.app)
	cloneIndex := map[string]*Node{}
	indexNodes(app.Node, nil, cloneIndex)

	// Everything that disappears from the old tree: explicit unloads plus
	// replaced definitions.
	detached := map[*Node]bool{}
	removals := []*registration{}
	for _, reg := range removeKeys {
		removals = append(removals, reg)
	}
	for _, plan := range plans {
		if !plan.unchanged && plan.replaces != nil {
			removals = append(removals, plan.replaces)
		}
	}
	sort.Slice(removals, func(i, j int) bool {
		return len(removals[i].path()) > len(removals[j].path())
	})
	for _, reg := range removals {
		if node := cloneIndex[joinPath(reg.path())]; node != nil {
			detached[node] = true
			detachNode(node)
			delete(cloneIndex, joinPath(reg.path()))
		}
	}

	// Add new subtrees shallowest-first so batch cross-dependencies resolve.
	additions := make([]*plannedCommand, 0, len(plans))
	for _, plan := range plans {
		if !plan.unchanged {
			additions = append(additions, plan)
		}
	}
	sort.Slice(additions, func(i, j int) bool {
		return len(additions[i].parent) < len(additions[j].parent)
	})
	for _, plan := range additions {
		parent := app.Node
		if len(plan.parent) > 0 {
			parent = cloneIndex[joinPath(plan.parent)]
		}
		if parent == nil {
			return nil, RegistryErrors{{
				Code:    UnknownParentError,
				Owner:   change.Owner,
				ID:      plan.identity.ID,
				Path:    append([]string(nil), plan.parent...),
				Message: fmt.Sprintf("internal error: mount point %s vanished while applying batch", strings.Join(plan.parent, " ")),
			}}
		}
		plan.node.Parent = parent
		parent.Children = append(parent.Children, plan.node)
		indexNodes(plan.node, plan.mountPath(), cloneIndex)
		if plan.isDefault {
			parent.DefaultCmd = plan.node
		}
		if err := r.k.interpolate(plan.node); err != nil {
			return nil, RegistryErrors{{
				Code:    InvalidCommandError,
				Owner:   change.Owner,
				ID:      plan.identity.ID,
				Path:    plan.mountPath(),
				Cause:   err,
				Message: fmt.Sprintf("command %s failed variable interpolation", plan.identity),
			}}
		}
	}

	// Carry surviving registrations forward, remapped onto cloned nodes.
	newRecords := map[*Node]*registration{}
	for oldNode, reg := range current.records {
		cp := cloner.nodes[oldNode]
		if cp == nil || detached[cp] {
			continue
		}
		newRecords[cp] = reg
	}

	unchangedSet := map[string]bool{}
	for _, plan := range plans {
		if plan.unchanged {
			reg := plan.replaces
			if cp := cloneIndex[joinPath(reg.path())]; cp != nil {
				newRecords[cp] = reg
			}
			unchangedSet[plan.identity.key()] = true
			result.Unchanged = append(result.Unchanged, plan.identity)
			continue
		}
		reg := plan.toRegistration(current.id + 1)
		if plan.replaces != nil {
			result.Replaced = append(result.Replaced, ReplacedCommand{
				Identity:           plan.identity,
				PreviousGeneration: plan.replaces.generation,
			})
		} else {
			result.Added = append(result.Added, plan.identity)
		}
		newRecords[plan.node] = reg
		r.records[plan.identity.key()] = reg
	}
	for key, reg := range removeKeys {
		if _, kept := unchangedSet[key]; kept {
			continue
		}
		if _, replaced := findPlanned(plans, key); replaced {
			continue
		}
		delete(r.records, key)
		result.Removed = append(result.Removed, reg.identity)
	}
	sortIdentities(result.Added)
	sortIdentities(result.Removed)
	sortIdentities(result.Unchanged)
	sort.Slice(result.Replaced, func(i, j int) bool {
		return result.Replaced[i].Identity.String() < result.Replaced[j].Identity.String()
	})

	version := &treeVersion{
		id:      current.id + 1,
		app:     app,
		records: newRecords,
	}
	version.rebuild()
	result.Generation = version.id
	r.versions[version.id] = version
	r.current.Store(version)
	r.nextID = version.id + 1
	// Sweep historical versions nobody has pinned. Pinned versions must be
	// retained so in-use unloads can be detected and reported.
	for id, old := range r.versions {
		if id != version.id && old.pins == 0 {
			delete(r.versions, id)
		}
	}
	return result, nil
}

func indexNodes(n *Node, parts []string, index map[string]*Node) {
	if n.Type == CommandNode {
		index[joinPath(parts)] = n
	}
	for _, child := range n.Children {
		indexNodes(child, append(parts, child.Name), index)
	}
}

func detachNode(node *Node) {
	parent := node.Parent
	if parent == nil {
		return
	}
	children := parent.Children[:0]
	for _, child := range parent.Children {
		if child != node {
			children = append(children, child)
		}
	}
	parent.Children = children
	if parent.DefaultCmd == node {
		parent.DefaultCmd = nil
	}
}

func findPlanned(plans []*plannedCommand, key string) (*plannedCommand, bool) {
	for _, plan := range plans {
		if plan.identity.key() == key {
			return plan, true
		}
	}
	return nil, false
}

func specsEqual(existing *registration, spec PluginCommand, name string, parent []string) bool {
	if existing.targetPtr != reflect.ValueOf(spec.Cmd).Pointer() {
		return false
	}
	if existing.name != name || existing.spec.Help != spec.Help || existing.spec.Group != spec.Group {
		return false
	}
	if strings.Join(existing.spec.Tags, "\x01") != strings.Join(spec.Tags, "\x01") {
		return false
	}
	return joinPath(existing.parent) == joinPath(parent)
}

func sortedRegistrationKeys(m map[string]*registration) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sortIdentities(ids []CommandIdentity) {
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
}

func dedupeRegistryErrors(errs []*RegistryError) RegistryErrors {
	seen := map[string]bool{}
	out := make(RegistryErrors, 0, len(errs))
	for _, e := range errs {
		key := string(e.Code) + "|" + e.Owner + "|" + e.ID + "|" + e.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}
