package kong

import (
	"reflect"
)

// treeCloner produces a private, structurally identical copy of a published
// command tree. Shared, frozen metadata (Tags, Mappers, Groups, slice fields
// that are never mutated after publishing) is shared; nodes, flags and
// values are freshly allocated so that parse-time state (Active, Set,
// Negated, Required rewrites, positional bookkeeping) is private to one
// parse and cannot race with other parses or registry commits.
type treeCloner struct {
	nodes  map[*Node]*Node
	values map[*Value]*Value
	flags  map[*Flag]*Flag
}

func newTreeCloner() *treeCloner {
	return &treeCloner{
		nodes:  map[*Node]*Node{},
		values: map[*Value]*Value{},
		flags:  map[*Flag]*Flag{},
	}
}

func (c *treeCloner) cloneApplication(app *Application) *Application {
	root := c.cloneNode(app.Node, nil)
	out := &Application{
		Node:     root,
		HelpFlag: nil,
	}
	if app.HelpFlag != nil {
		out.HelpFlag = c.flags[app.HelpFlag]
		// The help flag's target is allocated by Kong itself rather than
		// supplied through the host grammar, so give every parse a private
		// copy; concurrent parses would otherwise race Reset() on the single
		// shared help bool.
		if out.HelpFlag.Value.Target.IsValid() {
			out.HelpFlag.Value.Target = reflect.New(out.HelpFlag.Value.Target.Type()).Elem()
		}
	}
	return out
}

func (c *treeCloner) cloneNode(src *Node, parent *Node) *Node {
	if cp, ok := c.nodes[src]; ok {
		return cp
	}
	n := &Node{
		Type:          src.Type,
		Parent:        parent,
		Name:          src.Name,
		Help:          src.Help,
		Detail:        src.Detail,
		Group:         src.Group,
		Hidden:        src.Hidden,
		Tag:           src.Tag,
		Aliases:       src.Aliases,
		Passthrough:   src.Passthrough,
		Target:        src.Target,
		registryID:    src.registryID,
		legacyDynamic: src.legacyDynamic,
	}
	c.nodes[src] = n
	for _, flag := range src.Flags {
		n.Flags = append(n.Flags, c.cloneFlag(flag))
	}
	for _, positional := range src.Positional {
		n.Positional = append(n.Positional, c.cloneValue(positional))
	}
	for _, child := range src.Children {
		n.Children = append(n.Children, c.cloneNode(child, n))
	}
	if src.Argument != nil {
		n.Argument = c.values[src.Argument]
	}
	if src.DefaultCmd != nil {
		n.DefaultCmd = c.nodes[src.DefaultCmd]
	}
	return n
}

func (c *treeCloner) cloneFlag(src *Flag) *Flag {
	if cp, ok := c.flags[src]; ok {
		return cp
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
	}
	c.flags[src] = f
	f.Value = c.cloneValue(src.Value)
	f.Value.Flag = f
	return f
}

func (c *treeCloner) cloneValue(src *Value) *Value {
	if cp, ok := c.values[src]; ok {
		return cp
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
		Target:          src.Target,
		Required:        src.Required,
		Format:          src.Format,
		Passthrough:     src.Passthrough,
		PassthroughMode: src.PassthroughMode,
	}
	c.values[src] = v
	return v
}

// deepCopyValue duplicates a prepared command prototype for one parse.
// Structs, pointer-to-struct fields, struct slices and plugin interface
// values are duplicated; scalar pointers, maps and func values are shared,
// as they are either replaced wholesale during parsing or invoked
// read-only.
func deepCopyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return v
		}
		elem := v.Elem()
		nv := reflect.New(v.Type().Elem())
		if elem.Kind() == reflect.Struct {
			nv.Elem().Set(deepCopyValue(elem))
		} else {
			nv.Elem().Set(elem)
		}
		return nv
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		elem := v.Elem()
		if elem.Kind() == reflect.Ptr && !elem.IsNil() && elem.Elem().Kind() == reflect.Struct {
			nv := reflect.New(elem.Elem().Type())
			nv.Elem().Set(deepCopyValue(elem.Elem()))
			out := reflect.New(v.Type()).Elem()
			out.Set(nv)
			return out
		}
		return v
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if !field.CanSet() {
				continue
			}
			out.Field(i).Set(deepCopyValue(field))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(deepCopyValue(v.Index(i)))
		}
		return out
	default:
		return v
	}
}

// rebinder rewires a cloned runtime command subtree to point at a freshly
// allocated, deep-copied command target and its fields.
type rebinder struct {
	k *Kong
}

// rebind walks the published (prototype) node and its private clone in
// lockstep, assigning fresh reflect.Values for the root target, its flags
// and positional arguments, and its nested command structs. Subtrees owned
// by another registration are left untouched; they receive their own
// rebind pass.
func (rb *rebinder) rebind(src, cp *Node, rootKey string, fresh reflect.Value) {
	if src.registryID != "" && src.registryID != rootKey {
		return
	}
	cp.Target = fresh

	fields, err := flattenedFields(fresh, newEmptyTag())
	if err != nil {
		return
	}
	flagTargets := map[string]reflect.Value{}
	argTargets := map[string]reflect.Value{}
	branches := map[string]reflect.Value{}
	for _, field := range fields {
		name := field.tag.Name
		if name == "" {
			name = field.tag.Prefix + rb.k.flagNamer(field.field.Name)
		} else {
			name = field.tag.Prefix + name
		}
		structBranch := field.value.Kind() == reflect.Struct &&
			(field.tag.Cmd || field.tag.Arg) && rb.k.registry.ForValue(field.value) == nil
		if structBranch {
			kind := "c"
			if field.tag.Arg {
				kind = "a"
			}
			branches[kind+":"+name] = field.value
			continue
		}
		if field.tag.Arg {
			argTargets[name] = field.value
		} else {
			flagTargets[name] = field.value
		}
	}
	for _, flag := range cp.Flags {
		if target, ok := flagTargets[flag.Value.Name]; ok {
			flag.Value.Target = target
		}
	}
	for _, positional := range cp.Positional {
		if target, ok := argTargets[positional.Name]; ok {
			positional.Target = target
		}
	}
	if cp.Argument != nil {
		if target, ok := argTargets[cp.Name]; ok {
			cp.Argument.Target = target
		}
	}
	for i, child := range src.Children {
		if child.registryID != "" && child.registryID != rootKey {
			continue
		}
		kind := "c"
		if child.Type == ArgumentNode {
			kind = "a"
		}
		target, ok := branches[kind+":"+child.Name]
		if !ok || i >= len(cp.Children) {
			continue
		}
		rb.rebind(child, cp.Children[i], rootKey, target)
	}
}

// rebindDynamicNode replaces all targets in one cloned runtime subtree with
// private per-parse values derived from fresh.
func rebindDynamicNode(k *Kong, rootKey string, src, cp *Node, fresh reflect.Value) {
	rb := &rebinder{k: k}
	rb.rebind(src, cp, rootKey, fresh)
}
