package kong

import (
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
)

// Session is an isolated parse session built on top of an already constructed
// Kong grammar.
//
// Sessions are created explicitly with [Kong.NewSession]. Every session owns
// an independent copy of the grammar tree and parse target: its node/flag
// activity, flag "set" state, default-reset state, temporary token values,
// default-command selection, passthrough arguments, resolver products,
// validation inputs, bindings and output writers are all private to the
// session. A parse failure, cancellation or help invocation in one session can
// never affect another session or the base grammar.
//
// Because Session embeds *[Kong], [Kong.Parse] is available directly as
// Session.Parse. [Trace], [Context.Reset], [Context.Resolve],
// [Context.Validate], [Context.PrintUsage], [Context.Call] and [Context.Run]
// can all be driven independently per session.
//
// The framework isolates its own internal state. Components supplied by the
// caller and shared between sessions are not made thread-safe by Kong: custom
// [Mapper] implementations, [Resolver] instances, bound values and hook/Run
// functions that mutate shared state must themselves be safe for concurrent
// use, or be supplied per session through SessionBind/SessionResolvers and
// context bindings. Singleton providers are cached per session.
type Session struct {
	*Kong

	// target is the pointer to this session's independent grammar root.
	target any
}

// Target returns the pointer to the session-owned grammar root.
//
// Parse results are written into this value. Cast it to the concrete grammar
// type, e.g. session.Target().(*MyCLI).
func (s *Session) Target() any {
	return s.target
}

// Trace is a convenience wrapper around the package-level [Trace] function.
func (s *Session) Trace(args []string) (*Context, error) {
	return Trace(s.Kong, args)
}

// SessionOption configures an isolated [Session].
type SessionOption interface {
	applySession(k *Kong) error
}

type sessionOptionFunc func(k *Kong) error

func (f sessionOptionFunc) applySession(k *Kong) error { return f(k) }

// SessionWriters overrides the stdout and stderr writers for a session,
// allowing concurrent sessions to write to independent outputs.
func SessionWriters(stdout, stderr io.Writer) SessionOption {
	return sessionOptionFunc(func(k *Kong) error {
		k.Stdout = stdout
		k.Stderr = stderr
		return nil
	})
}

// SessionExit overrides the termination function used by a session (eg. when
// --help is handled). Server-style applications should always provide one so
// that displaying help never calls the default os.Exit.
func SessionExit(exit func(int)) SessionOption {
	return sessionOptionFunc(func(k *Kong) error {
		k.Exit = exit
		return nil
	})
}

// SessionBind binds values to a session for use by hooks and Run() arguments.
func SessionBind(args ...any) SessionOption {
	return sessionOptionFunc(func(k *Kong) error {
		k.bindings.add(args...)
		return nil
	})
}

// SessionBindTo binds an implementation to an interface within a session.
//
//	BindTo(impl, (*iface)(nil))
func SessionBindTo(impl, iface any) SessionOption {
	return sessionOptionFunc(func(k *Kong) error {
		k.bindings.addTo(impl, iface)
		return nil
	})
}

// SessionBindToProvider binds a provider function to a session.
//
// Unlike providers bound on the base Kong, a session provider is invoked and
// cached (when non-singleton) independently of every other session.
func SessionBindToProvider(provider any) SessionOption {
	return sessionOptionFunc(func(k *Kong) error {
		return k.bindings.addProvider(provider, false /* singleton */)
	})
}

// SessionBindSingletonProvider binds a singleton provider to a session. The
// provider result is cached within the session and never shared with other
// sessions.
func SessionBindSingletonProvider(provider any) SessionOption {
	return sessionOptionFunc(func(k *Kong) error {
		return k.bindings.addProvider(provider, true /* singleton */)
	})
}

// SessionResolvers appends resolvers private to a session.
//
// The resolver instances are invoked with the session's context; whatever they
// resolve is applied only to the session's target.
func SessionResolvers(resolvers ...Resolver) SessionOption {
	return sessionOptionFunc(func(k *Kong) error {
		k.resolvers = append(k.resolvers, resolvers...)
		return nil
	})
}

// NewSession creates an isolated parse session from an already built grammar.
//
// The base Kong is used as a read-only template: its model is deep-copied and
// retargeted onto a fresh copy of the grammar structure, so concurrent
// sessions never share targets, node/flag state, temporary parse values,
// default-command choices, passthrough tokens, resolver products, bindings or
// writers. The base grammar itself is never modified by sessions.
//
// Sessions may be created concurrently and each session may be parsed
// concurrently with other sessions. See [Session] for the thread-safety
// boundary around caller-supplied mappers, resolvers and bound values.
func (k *Kong) NewSession(options ...SessionOption) (*Session, error) {
	targets, root, keep, err := k.copyTargets()
	if err != nil {
		return nil, err
	}
	model, err := cloneApplication(k.Model, targets)
	if err != nil {
		return nil, err
	}

	sk := &Kong{
		Model:           model,
		Exit:            k.Exit,
		Stdout:          k.Stdout,
		Stderr:          k.Stderr,
		bindings:        k.bindings.cloneForSession(k),
		loader:          k.loader,
		resolvers:       append([]Resolver(nil), k.resolvers...),
		registry:        k.registry,
		ignoreFields:    append([]*regexp.Regexp(nil), k.ignoreFields...),
		noDefaultHelp:   k.noDefaultHelp,
		allowHyphenated: k.allowHyphenated,
		usageOnError:    k.usageOnError,
		help:            k.help,
		shortHelp:       k.shortHelp,
		helpFormatter:   k.helpFormatter,
		helpOptions:     k.helpOptions,
		helpFlag:        model.HelpFlag,
		groups:          k.groups,
		vars:            k.vars,
		flagNamer:       k.flagNamer,
		hooks:           cloneHooks(k.hooks),
		embedded:        k.embedded,
		dynamicCommands: k.dynamicCommands,
		sessionKeep:     keep,
	}
	// Bind(k) on the base parser registered the base *Kong; sessions must
	// inject themselves instead.
	sk.bindings.add(sk)

	for _, option := range options {
		if err := option.applySession(sk); err != nil {
			return nil, err
		}
	}

	return &Session{Kong: sk, target: root}, nil
}

// copyTargets builds a mapping from every reflect.Value used as a Node/Value
// target in the base grammar to an independent, settable copy. It also returns
// the session grammar root pointer and references that must be kept alive.
//
// The base model and base grammar struct are only read: field discovery is
// re-run on the fresh copies, so pointer command hydration performed by
// flattenedFields can never replace allocations inside the base grammar.
func (k *Kong) copyTargets() (map[reflect.Value]reflect.Value, any, []any, error) {
	targets := map[reflect.Value]reflect.Value{}
	keep := []any{}

	// Root grammar structure.
	origRoot := k.Model.Node.Target
	if !origRoot.IsValid() || origRoot.Kind() != reflect.Struct {
		return nil, nil, nil, fmt.Errorf("kong: session target must be a struct, got %s", origRoot.Type())
	}
	rootPtr := reflect.New(origRoot.Type())
	deepCopy(rootPtr.Elem(), origRoot)
	keep = append(keep, rootPtr.Interface())
	targets[origRoot] = rootPtr.Elem()

	// Fresh copies of structs registered through Embed()/DynamicCommand().
	// They are external to the root grammar; their flags and command nodes
	// were attached to the application node during New().
	extras := []rootExtra{}

	for _, embed := range k.embedded {
		ov := reflect.Indirect(reflect.ValueOf(embed.strct))
		freshPtr := reflect.New(ov.Type())
		deepCopy(freshPtr.Elem(), ov)
		keep = append(keep, freshPtr.Interface())
		tag, err := parseTagString(strings.Join(embed.tags, " "))
		if err != nil {
			return nil, nil, nil, err
		}
		tag.Embed = true
		fields, err := flattenedFields(freshPtr.Elem(), tag)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, field := range fields {
			extras = append(extras, rootExtra{
				name:   fieldName(k.flagNamer, field.tag, field.field.Name),
				isNode: field.value.Kind() == reflect.Struct && (field.tag.Cmd || field.tag.Arg),
				value:  field.value,
			})
		}
	}
	for _, dcmd := range k.dynamicCommands {
		ov := reflect.Indirect(reflect.ValueOf(dcmd.cmd))
		if !ov.IsValid() {
			continue
		}
		freshPtr := reflect.New(ov.Type())
		deepCopy(freshPtr.Elem(), ov)
		keep = append(keep, freshPtr.Interface())
		extras = append(extras, rootExtra{name: dcmd.name, isNode: true, value: freshPtr.Elem()})
	}

	if err := k.mirrorNode(k.Model.Node, rootPtr.Elem(), targets, extras); err != nil {
		return nil, nil, nil, err
	}

	// The synthetic help flag targets a private value allocated by extraFlags().
	if k.helpFlag != nil {
		helpPtr := reflect.New(k.helpFlag.Value.Target.Type())
		keep = append(keep, helpPtr.Interface())
		targets[k.helpFlag.Value.Target] = helpPtr.Elem()
	}

	return targets, rootPtr.Interface(), keep, nil
}

// fieldName reproduces the flag/command naming done by buildNode.
func fieldName(namer func(string) string, tag *Tag, goFieldName string) string {
	if tag.Name != "" {
		return tag.Prefix + tag.Name
	}
	return tag.Prefix + namer(goFieldName)
}

// rootExtra is a fresh value for an Embed()/DynamicCommand() contribution
// attached directly to the application node.
type rootExtra struct {
	name   string
	isNode bool
	value  reflect.Value
}

// mirrorNode registers fresh targets for a model node by re-running field
// discovery on the fresh struct copy and pairing results with model elements
// by final name (flags/commands) or declaration order (positionals).
func (k *Kong) mirrorNode(node *Node, fresh reflect.Value, targets map[reflect.Value]reflect.Value, extras []rootExtra) error {
	targets[node.Target] = fresh

	childrenByName := map[string]*Node{}
	for _, child := range node.Children {
		childrenByName[child.Name] = child
	}
	flagsByName := map[string]*Value{}
	for _, flag := range node.Flags {
		flagsByName[flag.Value.Name] = flag.Value
	}

	type candidate struct {
		name         string
		isNode       bool
		isPositional bool
		optional     bool
		value        reflect.Value
	}
	fields, err := flattenedFields(fresh, nodeMirrorTag(node))
	if err != nil {
		return err
	}
	candidates := []candidate{}
	for _, field := range fields {
		isNode := field.value.Kind() == reflect.Struct && (field.tag.Cmd || field.tag.Arg)
		candidates = append(candidates, candidate{
			name:         fieldName(k.flagNamer, field.tag, field.field.Name),
			isNode:       isNode,
			isPositional: field.tag.Arg && !isNode,
			value:        field.value,
		})
	}
	for _, extra := range extras {
		candidates = append(candidates, candidate{name: extra.name, isNode: extra.isNode, optional: true, value: extra.value})
	}

	// In a branching-argument node the first positional is promoted to
	// Node.Argument and removed from Node.Positional.
	positionals := node.Positional
	if node.Argument != nil {
		positionals = append([]*Value{node.Argument}, positionals...)
	}
	nextPositional := 0
	for _, cand := range candidates {
		if cand.isNode {
			if child := childrenByName[cand.name]; child != nil {
				if err := k.mirrorNode(child, cand.value, targets, nil); err != nil {
					return err
				}
				continue
			}
			// A struct carrying a custom mapper is built as a field rather
			// than a node; fall through to flag lookup.
		}
		if v := flagsByName[cand.name]; v != nil {
			targets[v.Target] = cand.value
			continue
		}
		if cand.isPositional && nextPositional < len(positionals) {
			targets[positionals[nextPositional].Target] = cand.value
			nextPositional++
			continue
		}
		// Contributions from Embed() only attach flags and command nodes to
		// the application node; anything else is not part of the model.
		if cand.optional {
			continue
		}
		return fmt.Errorf("kong: internal error cloning %s: unmatched field %q", node.Name, cand.name)
	}
	return nil
}

// nodeMirrorTag returns the parent tag that buildNode effectively used while
// flattening a node: buildChild passes a fresh tag carrying only EnvPrefix,
// while the application root is built with an empty tag.
func nodeMirrorTag(node *Node) *Tag {
	tag := newEmptyTag()
	if node.Parent != nil && node.Tag != nil {
		tag.EnvPrefix = node.Tag.EnvPrefix
	}
	return tag
}

// deepCopy recursively copies src into the settable destination dst,
// allocating fresh pointers, slices and maps. Interface data (such as Plugins
// elements) is copied concretely. Func, chan and scalar values are shared, as
// are unexported struct fields that cannot be set.
func deepCopy(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Ptr:
		if src.IsNil() {
			return
		}
		np := reflect.New(src.Type().Elem())
		deepCopy(np.Elem(), src.Elem())
		dst.Set(np)

	case reflect.Interface:
		if src.IsNil() {
			return
		}
		concrete := src.Elem()
		switch concrete.Kind() {
		case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Struct:
			cp := reflect.New(concrete.Type()).Elem()
			deepCopy(cp, concrete)
			dst.Set(cp)
		default:
			dst.Set(concrete)
		}

	case reflect.Slice:
		if src.IsNil() {
			return
		}
		ns := reflect.MakeSlice(src.Type(), src.Len(), src.Len())
		for i := 0; i < src.Len(); i++ {
			deepCopy(ns.Index(i), src.Index(i))
		}
		dst.Set(ns)

	case reflect.Map:
		if src.IsNil() {
			return
		}
		nm := reflect.MakeMapWithSize(src.Type(), src.Len())
		iter := src.MapRange()
		for iter.Next() {
			nk := reflect.New(iter.Key().Type()).Elem()
			deepCopy(nk, iter.Key())
			nv := reflect.New(iter.Value().Type()).Elem()
			deepCopy(nv, iter.Value())
			nm.SetMapIndex(nk, nv)
		}
		dst.Set(nm)

	case reflect.Struct:
		for i := 0; i < src.NumField(); i++ {
			if dst.Field(i).CanSet() {
				deepCopy(dst.Field(i), src.Field(i))
			}
		}

	default:
		dst.Set(src)
	}
}

func cloneHooks(src map[string][]reflect.Value) map[string][]reflect.Value {
	dst := make(map[string][]reflect.Value, len(src))
	for name, fns := range src {
		dst[name] = append([]reflect.Value(nil), fns...)
	}
	return dst
}

// cloneForSession copies bindings for an isolated session. Plain value
// bindings keep their (caller-owned) values; function/providers get a fresh
// per-session singleton cache. The *Kong entry is replaced with the session.
func (b bindings) cloneForSession(k *Kong) bindings {
	kongType := reflect.TypeOf(k)
	out := make(bindings, len(b)+1)
	for typ, bd := range b {
		if typ == kongType {
			continue
		}
		cp := *bd
		if bd.fn.IsValid() {
			// Drop any resolved singleton result so each session resolves
			// the provider independently.
			cp.done = false
			cp.val = reflect.Value{}
		}
		out[typ] = &cp
	}
	return out
}

// cloneApplication deep-copies the grammar tree, rewiring all internal pointers
// and retargeting every node and value onto targets.
func cloneApplication(app *Application, targets map[reflect.Value]reflect.Value) (*Application, error) {
	c := &modelCloner{
		targets: targets,
		nodes:   map[*Node]*Node{},
		values:  map[*Value]*Value{},
		flags:   map[*Flag]*Flag{},
	}
	root, err := c.node(app.Node)
	if err != nil {
		return nil, err
	}
	// Default commands live inside the child slice; now that every child is
	// cloned, rewire the references.
	if err := c.rewireDefaults(app.Node, root); err != nil {
		return nil, err
	}
	out := &Application{Node: root}
	if app.HelpFlag != nil {
		helpFlag, ok := c.flags[app.HelpFlag]
		if !ok {
			return nil, fmt.Errorf("kong: internal error cloning help flag")
		}
		out.HelpFlag = helpFlag
	}
	return out, nil
}

type modelCloner struct {
	targets map[reflect.Value]reflect.Value
	nodes   map[*Node]*Node
	values  map[*Value]*Value
	flags   map[*Flag]*Flag
}

func (c *modelCloner) retarget(v reflect.Value, what string) (reflect.Value, error) {
	nv, ok := c.targets[v]
	if !ok {
		return reflect.Value{}, fmt.Errorf("kong: internal error: no session target for %s %s", what, v.Type())
	}
	return nv, nil
}

func (c *modelCloner) node(src *Node) (*Node, error) {
	if src == nil {
		return nil, nil
	}
	if n, ok := c.nodes[src]; ok {
		return n, nil
	}
	target, err := c.retarget(src.Target, "node")
	if err != nil {
		return nil, err
	}
	n := &Node{
		Type:        src.Type,
		Name:        src.Name,
		Help:        src.Help,
		Detail:      src.Detail,
		Group:       src.Group,
		Hidden:      src.Hidden,
		DefaultCmd:  nil, // rewired after children are cloned
		Target:      target,
		Tag:         src.Tag,
		Aliases:     src.Aliases,
		Passthrough: src.Passthrough,
	}
	c.nodes[src] = n

	// A branching argument's value is removed from Positional at build time
	// and lives solely in Node.Argument, so clone it explicitly first.
	if src.Argument != nil {
		arg, err := c.value(src.Argument)
		if err != nil {
			return nil, err
		}
		n.Argument = arg
	}
	for _, flag := range src.Flags {
		nf, err := c.flag(flag)
		if err != nil {
			return nil, err
		}
		n.Flags = append(n.Flags, nf)
	}
	for _, pos := range src.Positional {
		nv, err := c.value(pos)
		if err != nil {
			return nil, err
		}
		n.Positional = append(n.Positional, nv)
	}
	for _, child := range src.Children {
		nc, err := c.node(child)
		if err != nil {
			return nil, err
		}
		nc.Parent = n
		n.Children = append(n.Children, nc)
	}
	return n, nil
}

func (c *modelCloner) rewireDefaults(src, dst *Node) error {
	if src.DefaultCmd != nil {
		def, ok := c.nodes[src.DefaultCmd]
		if !ok {
			return fmt.Errorf("kong: internal error cloning default command of %s", src.Name)
		}
		dst.DefaultCmd = def
	}
	for i, child := range src.Children {
		if err := c.rewireDefaults(child, dst.Children[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *modelCloner) flag(src *Flag) (*Flag, error) {
	if f, ok := c.flags[src]; ok {
		return f, nil
	}
	f := &Flag{
		Group:       src.Group,
		Xor:         src.Xor,
		LastWins:    src.LastWins,
		And:         src.And,
		PlaceHolder: src.PlaceHolder,
		Envs:        src.Envs,
		Aliases:     src.Aliases,
		Short:       src.Short,
		Hidden:      src.Hidden,
		// Negated is parse state, like Set/Active, and starts clean.
	}
	c.flags[src] = f
	nv, err := c.value(src.Value)
	if err != nil {
		return nil, err
	}
	f.Value = nv
	nv.Flag = f
	return f, nil
}

func (c *modelCloner) value(src *Value) (*Value, error) {
	if v, ok := c.values[src]; ok {
		return v, nil
	}
	target, err := c.retarget(src.Target, "value")
	if err != nil {
		return nil, err
	}
	v := &Value{
		Name:            src.Name,
		Help:            src.Help,
		OrigHelp:        src.OrigHelp,
		HasDefault:      src.HasDefault,
		Default:         src.Default,
		DefaultValue:    src.DefaultValue,
		Enum:            src.Enum,
		Mapper:          src.Mapper,
		Tag:             src.Tag,
		Target:          target,
		Required:        src.Required,
		Format:          src.Format,
		Position:        src.Position,
		Passthrough:     src.Passthrough,
		PassthroughMode: src.PassthroughMode,
		// Set and Active intentionally reset: parse state must never leak
		// out of a session.
	}
	c.values[src] = v
	return v, nil
}
