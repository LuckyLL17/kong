package kong

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// Profile precedence, from highest to lowest:
//
//  1. Flags given directly on the command line.
//  2. Resolvers added through Context.AddResolver().
//  3. Resolvers registered with Resolvers().
//  4. The selected profile (child layers override parent layers; within one
//     layer, a value scoped to a deeper command path overrides one scoped to a
//     shallower path, which overrides a global value).
//  5. Resolvers created from Configuration() files.
//  6. Environment variables.
//  7. Defaults from the "default" tag.
//
// When no profile set is configured, or no profile is selected, the ordering
// above is not applied and Kong behaves exactly as without profiles.

// ProfileErrorKind identifies the kind of configuration profile error.
type ProfileErrorKind int

// Profile error kinds reported by profile parsing, compilation and selection.
const (
	// ProfileInvalidDocument: the profile document is malformed JSON or uses
	// unknown keys.
	ProfileInvalidDocument ProfileErrorKind = iota
	// ProfileUnknown: a referenced or selected profile does not exist.
	ProfileUnknown
	// ProfileCycle: parent references form an inheritance cycle.
	ProfileCycle
	// ProfileDuplicateParent: a profile names the same parent more than once,
	// specifies both "parent" and "parents", or reaches the same parent twice
	// through its inheritance graph.
	ProfileDuplicateParent
	// ProfileUnknownScope: a command path used as a value scope does not exist.
	ProfileUnknownScope
	// ProfileUnknownField: a profile sets a key that is not a flag of the
	// application.
	ProfileUnknownField
	// ProfileFieldAtOtherCommand: a scoped value names a flag that is only
	// declared on a command the scope does not pass through.
	ProfileFieldAtOtherCommand
	// ProfileTypeMismatch: a profile value cannot be decoded into the flag.
	ProfileTypeMismatch
	// ProfileInvalidEnum: a profile value is not one of the flag's enums.
	ProfileInvalidEnum
	// ProfileXorConflict: a profile chain supplies two flags of one xor group
	// for command paths that can be selected together.
	ProfileXorConflict
)

// ProfileError describes a profile problem with enough context to locate it in
// the profile inheritance chain and the target command path.
type ProfileError struct {
	// Kind classifies the error.
	Kind ProfileErrorKind
	// Profile is the profile the error was found in.
	Profile string
	// Chain is the resolved inheritance chain (root first), when available.
	Chain []string
	// Command is the command path the error applies to ("" for the root).
	Command string
	// Field is the flag key causing the error.
	Field string
	// Detail carries free-form context.
	Detail string
	// Cause is the underlying error, if any.
	Cause error
}

func (e *ProfileError) Error() string {
	var b strings.Builder
	b.WriteString("profiles: ")
	switch e.Kind {
	case ProfileInvalidDocument:
		b.WriteString("invalid profile document")
	case ProfileUnknown:
		b.WriteString("unknown profile ")
		b.WriteString(strconvQuote(e.Profile))
	case ProfileCycle:
		b.WriteString("inheritance cycle detected")
	case ProfileDuplicateParent:
		b.WriteString("duplicate parent in inheritance chain")
	case ProfileUnknownScope:
		b.WriteString("unknown command scope ")
		b.WriteString(strconvQuote(e.Command))
	case ProfileUnknownField:
		b.WriteString("unknown field ")
		b.WriteString(strconvQuote(e.Field))
	case ProfileFieldAtOtherCommand:
		b.WriteString("field ")
		b.WriteString(strconvQuote(e.Field))
		b.WriteString(" is not available at this command scope")
	case ProfileTypeMismatch:
		b.WriteString("invalid value for field ")
		b.WriteString(strconvQuote(e.Field))
	case ProfileInvalidEnum:
		b.WriteString("invalid enum value for field ")
		b.WriteString(strconvQuote(e.Field))
	case ProfileXorConflict:
		b.WriteString("mutually exclusive flags set by profile chain")
	default:
		b.WriteString("profile error")
	}
	if e.Profile != "" && e.Kind != ProfileUnknown {
		b.WriteString(" in profile ")
		b.WriteString(strconvQuote(e.Profile))
	}
	if len(e.Chain) > 0 {
		b.WriteString(" (chain ")
		b.WriteString(strings.Join(e.Chain, " -> "))
		b.WriteString(")")
	}
	if e.Command != "" && e.Kind != ProfileUnknownScope {
		b.WriteString(" for command ")
		b.WriteString(strconvQuote(e.Command))
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *ProfileError) Unwrap() error { return e.Cause }

func strconvQuote(s string) string {
	return fmt.Sprintf("%q", s)
}

func profileErr(kind ProfileErrorKind, cause error, format string, args ...any) *ProfileError {
	return &ProfileError{Kind: kind, Cause: cause, Detail: fmt.Sprintf(format, args...)}
}

// ProfileSet is an immutable, parsed set of named configuration profiles.
//
// A profile document is JSON with this shape:
//
//	{
//	  "default_profile": "dev",
//	  "profiles": {
//	    "base": {
//	      "values": {"addr": ":8000"}
//	    },
//	    "dev": {
//	      "parent": "base",
//	      "values": {"addr": ":8080"},
//	      "commands": {
//	        "serve": {"addr": ":9090"},
//	        "serve tls": {"cert": "/tmp/cert.pem"}
//	      }
//	    }
//	  }
//	}
//
// "values" apply globally, exactly like a JSON configuration file. "commands"
// maps a space (or dot) separated command path to values that only take effect
// when that command path is selected. Flag keys follow the same conventions as
// kong.JSON: kebab-case, snake_case and nested or dotted objects are accepted.
type ProfileSet struct {
	doc profileDocument
}

type profileDocument struct {
	DefaultProfile string                     `json:"default_profile"`
	Profiles       map[string]profileLayerDoc `json:"profiles"`
}

type profileLayerDoc struct {
	Parent      string                    `json:"parent"`
	Parents     []string                  `json:"parents"`
	Description string                    `json:"description"`
	Values      map[string]any            `json:"values"`
	Commands    map[string]map[string]any `json:"commands"`
}

// ParseProfiles parses and structurally validates a profile document from r.
// The returned set is not bound to an application until it is installed with
// Profiles() or Kong.ReplaceProfiles(); that is where flags, enums and command
// scopes are validated.
func ParseProfiles(r io.Reader) (*ProfileSet, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var doc profileDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, profileErr(ProfileInvalidDocument, err, "%s", err.Error())
	}
	if doc.Profiles == nil {
		return nil, profileErr(ProfileInvalidDocument, nil, `document must contain a "profiles" object`)
	}
	if err := validateProfileDocument(doc); err != nil {
		return nil, err
	}
	return &ProfileSet{doc: doc}, nil
}

func validateProfileDocument(doc profileDocument) error {
	errs := []error{}
	if _, ok := doc.Profiles[""]; ok {
		errs = append(errs, profileErr(ProfileInvalidDocument, nil, "profile names must not be empty"))
	}
	for name, layer := range doc.Profiles {
		if name == "" {
			continue
		}
		if layer.Parent != "" && len(layer.Parents) > 0 {
			errs = append(errs, &ProfileError{
				Kind:    ProfileDuplicateParent,
				Profile: name,
				Detail:  `specify either "parent" or "parents", not both`,
			})
		}
		seen := map[string]bool{}
		for _, parent := range layer.Parents {
			if parent == "" {
				errs = append(errs, &ProfileError{
					Kind:    ProfileInvalidDocument,
					Profile: name,
					Detail:  "parent names must not be empty",
				})
			}
			if seen[parent] {
				errs = append(errs, &ProfileError{
					Kind:    ProfileDuplicateParent,
					Profile: name,
					Field:   parent,
					Detail:  fmt.Sprintf("parent %q is listed more than once", parent),
				})
			}
			seen[parent] = true
		}
		if layer.Parent != strings.TrimSpace(layer.Parent) {
			errs = append(errs, &ProfileError{
				Kind:    ProfileInvalidDocument,
				Profile: name,
				Detail:  "parent name must not contain surrounding whitespace",
			})
		}
		for scope := range layer.Commands {
			if strings.TrimSpace(scope) == "" {
				errs = append(errs, &ProfileError{
					Kind:    ProfileInvalidDocument,
					Profile: name,
					Detail:  "command scopes must not be empty",
				})
			}
		}
	}
	return errors.Join(errs...)
}

// compiledLayer is a profile layer bound to the command tree.
type compiledLayer struct {
	name    string
	parents []string
	global  map[string]any
	// scopes maps a canonical command path ("" is the root) to its values.
	scopes map[string]map[string]any
	// scopeLabels preserves the command path in document order for diagnostics.
	scopeLabels []string
}

type compiledProfileSet struct {
	doc    profileDocument
	layers map[string]*compiledLayer
	order  []string
}

// compileProfiles binds a parsed profile document to an application and runs
// every validation that requires the command tree. It never mutates an
// installed set: callers replace the installed set only after this succeeds.
func compileProfiles(doc profileDocument, app *Application) (*compiledProfileSet, error) {
	set := &compiledProfileSet{
		doc:    doc,
		layers: map[string]*compiledLayer{},
		order:  make([]string, 0, len(doc.Profiles)),
	}
	errs := []error{}
	for name := range doc.Profiles {
		set.order = append(set.order, name)
	}
	sort.Strings(set.order)

	for _, name := range set.order {
		layer := doc.Profiles[name]
		compiled := &compiledLayer{
			name:    name,
			global:  layer.Values,
			scopes:  map[string]map[string]any{},
			parents: []string{},
		}
		switch {
		case layer.Parent != "":
			compiled.parents = []string{layer.Parent}
		default:
			compiled.parents = append(compiled.parents, layer.Parents...)
		}
		for _, parent := range compiled.parents {
			if _, ok := doc.Profiles[parent]; !ok {
				errs = append(errs, &ProfileError{
					Kind:    ProfileUnknown,
					Profile: parent,
					Chain:   []string{name},
					Detail:  fmt.Sprintf("parent of profile %q does not exist", name),
				})
			}
		}
		scopes := make([]string, 0, len(layer.Commands))
		for scope := range layer.Commands {
			scopes = append(scopes, scope)
		}
		sort.Strings(scopes)
		for _, scope := range scopes {
			node, err := resolveScopeNode(app.Node, scope)
			if err != nil {
				errs = append(errs, &ProfileError{
					Kind:    ProfileUnknownScope,
					Profile: name,
					Command: scope,
					Cause:   err,
				})
				continue
			}
			canonical := node.Path()
			if _, exists := compiled.scopes[canonical]; exists {
				errs = append(errs, &ProfileError{
					Kind:    ProfileInvalidDocument,
					Profile: name,
					Command: canonical,
					Detail:  fmt.Sprintf("command scope %q is declared more than once", scope),
				})
				continue
			}
			compiled.scopes[canonical] = layer.Commands[scope]
			compiled.scopeLabels = append(compiled.scopeLabels, canonical)
		}
		set.layers[name] = compiled
	}

	if doc.DefaultProfile != "" {
		if _, ok := set.layers[doc.DefaultProfile]; !ok {
			errs = append(errs, &ProfileError{
				Kind:    ProfileUnknown,
				Profile: doc.DefaultProfile,
				Detail:  "default_profile does not name an existing profile",
			})
		}
	}

	chains := map[string][]string{}
	for _, name := range set.order {
		chain, err := set.linearize(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		chains[name] = chain
	}

	for _, name := range set.order {
		compiled := set.layers[name]
		if err := validateLayerValues(compiled, app); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range set.order {
		chain, ok := chains[name]
		if !ok {
			continue
		}
		if err := validateChainConflicts(set, chain, app); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return set, nil
}

func resolveScopeNode(root *Node, scope string) (*Node, error) {
	segments := splitScope(scope)
	if len(segments) == 0 {
		return nil, errors.New("empty command path")
	}
	node := root
	for _, segment := range segments {
		next := childCommand(node, segment)
		if next != nil && next.Type == CommandNode {
			node = next
			continue
		}
		candidates := []string{}
		for _, child := range node.Children {
			if child.Type == CommandNode {
				candidates = append(candidates, child.Name)
			}
		}
		return nil, findPotentialCandidates(segment, candidates, "unknown command %s", segment)
	}
	return node, nil
}

func splitScope(scope string) []string {
	return strings.FieldsFunc(scope, func(r rune) bool {
		return r == '.' || r == ' ' || r == '\t'
	})
}

// childCommand returns the command child matching name or one of its aliases.
func childCommand(node *Node, name string) *Node {
	for _, child := range node.Children {
		if child.Type != CommandNode {
			continue
		}
		if child.Name == name {
			return child
		}
		for _, alias := range child.Aliases {
			if alias == name {
				return child
			}
		}
	}
	return nil
}

func pathSegments(path string) []string {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return strings.Split(path, " ")
}

// pathHasPrefix reports whether the command path starts with prefix, compared
// segment by segment. The root path "" is a prefix of every path.
func pathHasPrefix(path, prefix string) bool {
	a := pathSegments(path)
	b := pathSegments(prefix)
	if len(b) > len(a) {
		return false
	}
	for i := range b {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// linearize resolves the inheritance chain of name as a root-first list in
// which each profile appears at most once.
func (s *compiledProfileSet) linearize(name string) ([]string, error) {
	out := []string{}
	onStack := map[string]bool{}
	seen := map[string]bool{}
	var visit func(string, []string) error
	visit = func(current string, stack []string) error {
		layer, ok := s.layers[current]
		if !ok {
			return &ProfileError{Kind: ProfileUnknown, Profile: current, Chain: stack}
		}
		if onStack[current] {
			return &ProfileError{
				Kind:    ProfileCycle,
				Profile: current,
				Chain:   append(append([]string{}, stack...), current),
				Detail:  "profile inherits from itself through the listed chain",
			}
		}
		if seen[current] {
			return &ProfileError{
				Kind:    ProfileDuplicateParent,
				Profile: current,
				Chain:   append(append([]string{}, stack...), current),
				Detail:  "profile is reached more than once through its parents",
			}
		}
		onStack[current] = true
		for _, parent := range layer.parents {
			if err := visit(parent, append(append([]string{}, stack...), current)); err != nil {
				return err
			}
		}
		onStack[current] = false
		seen[current] = true
		out = append(out, current)
		return nil
	}
	if err := visit(name, nil); err != nil {
		return nil, err
	}
	return out, nil
}

func allFlags(app *Application) map[*Flag]*Node {
	flags := map[*Flag]*Node{}
	var walk func(*Node)
	walk = func(node *Node) {
		for _, flag := range node.Flags {
			flags[flag] = node
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(app.Node)
	return flags
}

func visibleFlags(node *Node) []*Flag {
	var flags []*Flag
	for _, group := range node.AllFlags(false) {
		flags = append(flags, group...)
	}
	return flags
}

func validateLayerValues(layer *compiledLayer, app *Application) error {
	errs := []error{}
	everything := allFlags(app)
	if err := validateValueMap(layer, "", layer.global, nil, app); err != nil {
		errs = append(errs, err)
	}
	for _, scope := range layer.scopeLabels {
		scopeNode := findCommandNode(app.Node, scope)
		if scopeNode == nil {
			continue
		}
		visible := map[*Flag]*Node{}
		for _, flag := range visibleFlags(scopeNode) {
			visible[flag] = everything[flag]
		}
		if err := validateValueMap(layer, scope, layer.scopes[scope], visible, app); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func findCommandNode(root *Node, path string) *Node {
	node := root
	for _, segment := range pathSegments(path) {
		next := childCommand(node, segment)
		if next == nil {
			return nil
		}
		node = next
	}
	return node
}

// validateValueMap validates one values object. When candidates is nil every
// flag of the application is a candidate (global values); otherwise only the
// flags visible at the scope node are candidates.
func validateValueMap(layer *compiledLayer, scope string, values map[string]any,
	candidates map[*Flag]*Node, app *Application,
) error {
	if len(values) == 0 {
		return nil
	}
	errs := []error{}
	consumed := map[string]bool{}
	candidateFlags := []*Flag{}
	if candidates == nil {
		for flag := range allFlags(app) {
			candidateFlags = append(candidateFlags, flag)
		}
	} else {
		for flag := range candidates {
			candidateFlags = append(candidateFlags, flag)
		}
	}
	sort.Slice(candidateFlags, func(i, j int) bool {
		return candidateFlags[i].Name < candidateFlags[j].Name
	})
	for _, flag := range candidateFlags {
		raw, ok, paths := lookupConfigValueTracked(values, flag)
		if !ok {
			continue
		}
		for _, p := range paths {
			consumed[p] = true
		}
		if err := validateProfileRaw(flag, raw); err != nil {
			var perr *ProfileError
			if errors.As(err, &perr) {
				perr.Profile = layer.name
				perr.Command = scope
				errs = append(errs, perr)
				continue
			}
			errs = append(errs, &ProfileError{
				Kind:    ProfileTypeMismatch,
				Profile: layer.name,
				Command: scope,
				Field:   flag.Name,
				Cause:   err,
			})
		}
	}
	everything := allFlags(app)
	for _, unknown := range unconsumedLeaves(values, consumed) {
		errs = append(errs, unknownFieldError(layer.name, scope, unknown, everything, app))
	}
	return errors.Join(errs...)
}

func unknownFieldError(profile, scope, key string, everything map[*Flag]*Node, app *Application) error {
	flagKey := strings.ReplaceAll(key, "_", "-")
	owners := []string{}
	for flag, node := range everything {
		if flag.Name == key || flag.Name == flagKey || snakeCase(flag.Name) == key {
			owners = append(owners, node.Path())
		}
	}
	if len(owners) == 0 {
		return &ProfileError{
			Kind:    ProfileUnknownField,
			Profile: profile,
			Command: scope,
			Field:   key,
			Detail:  "no flag with this name exists",
		}
	}
	sort.Strings(owners)
	detail := fmt.Sprintf("the flag is only declared at command %s", strings.Join(quoteAll(uniqueSorted(owners)), ", "))
	if scope != "" {
		scopeNode := findCommandNode(app.Node, scope)
		if scopeNode != nil && scopeNode.DefaultCmd != nil {
			detail += fmt.Sprintf("; note that %q is the default command of %q", scopeNode.DefaultCmd.Name, scope)
		}
	}
	return &ProfileError{
		Kind:    ProfileFieldAtOtherCommand,
		Profile: profile,
		Command: scope,
		Field:   key,
		Detail:  detail,
	}
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = strconvQuote(v)
	}
	return out
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return values
	}
	sort.Strings(values)
	out := values[:1]
	for _, v := range values[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// validateProfileRaw decodes raw into a throwaway target of the flag's type and
// checks enum restrictions, without touching application state.
func validateProfileRaw(flag *Flag, raw any) error {
	target := reflect.New(flag.Value.Target.Type()).Elem()
	if target.Kind() == reflect.Ptr && target.IsNil() {
		target.Set(reflect.New(target.Type().Elem()))
	}
	scan := Scan().PushTyped(raw, FlagValueToken)
	if err := flag.Value.Mapper.Decode(&DecodeContext{Value: flag.Value, Scan: scan}, target); err != nil {
		return &ProfileError{
			Kind:  ProfileTypeMismatch,
			Field: flag.Name,
			Detail: fmt.Sprintf("value %s is not assignable to --%s",
				fmt.Sprintf("%v", raw), flag.Name),
			Cause: err,
		}
	}
	if flag.Value.Enum != "" {
		if err := checkEnum(flag.Value, target); err != nil {
			return &ProfileError{
				Kind:   ProfileInvalidEnum,
				Field:  flag.Name,
				Detail: err.Error(),
				Cause:  err,
			}
		}
	}
	return nil
}

// unconsumedLeaves walks values and reports dotted paths of leaves that no
// matched flag lookup consumed.
func unconsumedLeaves(values map[string]any, consumed map[string]bool) []string {
	leaves := []string{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			value := m[k]
			if nested, ok := value.(map[string]any); ok && !consumed[path] {
				walk(path, nested)
				continue
			}
			if !consumed[path] {
				leaves = append(leaves, path)
			}
		}
	}
	walk("", values)
	return leaves
}

// lookupConfigValue resolves a flag using the same key rules as kong.JSON,
// reporting every dotted map path a successful lookup touched so that unknown
// keys can be detected.
func lookupConfigValueTracked(values map[string]any, flag *Flag) (any, bool, []string) {
	if raw, ok := values[flag.Name]; ok {
		return raw, true, []string{flag.Name}
	}
	name := strings.ReplaceAll(flag.Name, "-", "_")
	if raw, ok := values[name]; ok {
		return raw, true, []string{name}
	}
	snake := snakeCase(flag.Name)
	if raw, ok := values[snake]; ok {
		return raw, true, []string{snake}
	}
	raw := any(values)
	path := []string{}
	for _, part := range strings.Split(name, ".") {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		raw, ok = m[part]
		if !ok {
			return nil, false, nil
		}
		path = append(path, part)
	}
	return raw, true, []string{strings.Join(path, ".")}
}

func lookupConfigValue(values map[string]any, flag *Flag) (any, bool) {
	raw, ok, _ := lookupConfigValueTracked(values, flag)
	return raw, ok
}

// profileProvision is one value a profile layer supplies for a flag.
type profileProvision struct {
	layer int
	scope string
}

// validateChainConflicts statically checks xor groups. For every selectable
// command context, the chain values active at that context must not set two
// members of one xor group declared on the same node.
func validateChainConflicts(set *compiledProfileSet, chain []string, app *Application) error {
	layers := make([]*compiledLayer, 0, len(chain))
	for _, name := range chain {
		layers = append(layers, set.layers[name])
	}
	contexts := []string{""}
	seenContext := map[string]bool{"": true}
	for _, layer := range layers {
		for _, scope := range layer.scopeLabels {
			if !seenContext[scope] {
				seenContext[scope] = true
				contexts = append(contexts, scope)
			}
		}
	}
	sort.Strings(contexts[1:])
	errs := []error{}
	var walk func(*Node)
	walk = func(node *Node) {
		for _, context := range contexts {
			errs = append(errs, xorConflictsAt(layers, chain, node, context)...)
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(app.Node)
	return errors.Join(errs...)
}

func xorConflictsAt(layers []*compiledLayer, chain []string,
	node *Node, context string,
) []error {
	groups := map[string]map[string]profileProvision{}
	for _, flag := range node.Flags {
		provision, ok := provisionFor(layers, flag, node.Path(), context)
		if !ok {
			continue
		}
		for _, group := range flag.Xor {
			if groups[group] == nil {
				groups[group] = map[string]profileProvision{}
			}
			groups[group][flag.Name] = provision
		}
	}
	errs := []error{}
	groupNames := make([]string, 0, len(groups))
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	sort.Strings(groupNames)
	for _, group := range groupNames {
		members := groups[group]
		if len(members) < 2 {
			continue
		}
		names := make([]string, 0, len(members))
		for name := range members {
			names = append(names, name)
		}
		sort.Strings(names)
		first, second := members[names[0]], members[names[1]]
		errs = append(errs, &ProfileError{
			Kind:    ProfileXorConflict,
			Profile: layers[first.layer].name,
			Chain:   chain,
			Command: context,
			Field:   names[0],
			Detail: fmt.Sprintf("--%s (profile %q%s) and --%s (profile %q%s) can't be set together (xor group %q)",
				names[0], layers[first.layer].name, scopeSuffix(first.scope),
				names[1], layers[second.layer].name, scopeSuffix(second.scope),
				group),
		})
	}
	return errs
}

func scopeSuffix(scope string) string {
	if scope == "" {
		return ""
	}
	return ", scope " + strconvQuote(scope)
}

// provisionFor resolves the winning chain provision for flag at the given
// owner node and selected command context, mirroring runtime precedence.
func provisionFor(layers []*compiledLayer, flag *Flag, ownerPath, context string) (profileProvision, bool) {
	var provision profileProvision
	found := false
	for i, layer := range layers {
		if scope, ok := layerScope(layer, flag, ownerPath, context); ok {
			provision = profileProvision{layer: i, scope: scope}
			found = true
			continue
		}
		if _, ok := lookupConfigValue(layer.global, flag); ok {
			provision = profileProvision{layer: i, scope: ""}
			found = true
		}
	}
	return provision, found
}

// layerScope returns the deepest scope in layer that applies to a flag owned at
// ownerPath for the selected command context.
func layerScope(layer *compiledLayer, flag *Flag, ownerPath, context string) (string, bool) {
	scope := ""
	found := false
	for _, candidate := range layer.scopeLabels {
		if !pathHasPrefix(candidate, ownerPath) {
			continue
		}
		if !pathHasPrefix(context, candidate) {
			continue
		}
		if _, ok := lookupConfigValue(layer.scopes[candidate], flag); !ok {
			continue
		}
		if !found || len(pathSegments(candidate)) > len(pathSegments(scope)) {
			scope = candidate
			found = true
		}
	}
	return scope, found
}

// profileHub holds the installed compiled set and the selected profile. It is
// safe for concurrent use; updates replace the whole set atomically.
type profileHub struct {
	mu       sync.RWMutex
	compiled *compiledProfileSet
	selected string
}

func newProfileHub() *profileHub {
	return &profileHub{}
}

func (h *profileHub) install(set *compiledProfileSet) {
	h.mu.Lock()
	h.compiled = set
	h.mu.Unlock()
}

func (h *profileHub) selectProfile(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.compiled == nil {
		return &ProfileError{Kind: ProfileUnknown, Profile: name, Detail: "no profile set is installed"}
	}
	if _, ok := h.compiled.layers[name]; !ok {
		return &ProfileError{
			Kind:    ProfileUnknown,
			Profile: name,
			Detail:  "available profiles: " + strings.Join(h.compiled.order, ", "),
		}
	}
	h.selected = name
	return nil
}

func (h *profileHub) activeSelection() (*compiledProfileSet, string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.compiled == nil {
		return nil, "", false
	}
	name := h.selected
	defaultSelected := false
	if name == "" {
		name = h.compiled.doc.DefaultProfile
		defaultSelected = name != ""
	}
	if name == "" {
		return h.compiled, "", false
	}
	return h.compiled, name, defaultSelected
}

// profileResolver is the per-context resolver installed between configuration
// file resolvers and registered global resolvers.
type profileResolver struct {
	selected        string
	defaultSelected bool
	chain           []*compiledLayer
	chainNames      []string
	supplied        map[*Flag]profileProvision
}

func newProfileResolver(set *compiledProfileSet, selected string, defaultSelected bool) (*profileResolver, error) {
	names, err := set.linearize(selected)
	if err != nil {
		return nil, err
	}
	chain := make([]*compiledLayer, 0, len(names))
	for _, name := range names {
		chain = append(chain, set.layers[name])
	}
	return &profileResolver{
		selected:        selected,
		defaultSelected: defaultSelected,
		chain:           chain,
		chainNames:      names,
		supplied:        map[*Flag]profileProvision{},
	}, nil
}

func (r *profileResolver) Validate(*Application) error { return nil }

func (r *profileResolver) Resolve(context *Context, parent *Path, flag *Flag) (any, error) {
	owner := parent.Node()
	ownerPath := ""
	if owner != nil {
		ownerPath = owner.Path()
	}
	selectedPath := ""
	if selected := context.Selected(); selected != nil {
		selectedPath = selected.Path()
	}
	provision, ok := provisionFor(r.chain, flag, ownerPath, selectedPath)
	if !ok {
		return nil, nil
	}
	raw := r.provisionValue(flag, provision)
	r.supplied[flag] = provision
	return raw, nil
}

func (r *profileResolver) provisionValue(flag *Flag, provision profileProvision) any {
	layer := r.chain[provision.layer]
	if provision.scope != "" {
		if raw, ok := lookupConfigValue(layer.scopes[provision.scope], flag); ok {
			return raw
		}
	}
	raw, _ := lookupConfigValue(layer.global, flag)
	return raw
}

// ProfileValueExplanation describes one value contributed by the selected
// profile chain.
type ProfileValueExplanation struct {
	// Flag is the resolved flag name.
	Flag string
	// Value is the raw profile value.
	Value any
	// Profile is the layer contributing the value.
	Profile string
	// Scope is the command path the value is scoped to ("" for global values).
	Scope string
	// Command is the command path of the node owning the flag.
	Command string
	// Active reports whether the value applies to the selected command path.
	Active bool
	// Effective reports whether the value is the final source of the flag.
	Effective bool
	// OverriddenBy explains why an active value did not take effect.
	OverriddenBy string
}

// ProfileExplanation is the public explanation of how the selected profile
// chain contributes to the resolved configuration.
type ProfileExplanation struct {
	// Selected is the profile used for resolution.
	Selected string
	// DefaultSelected reports whether Selected came from "default_profile".
	DefaultSelected bool
	// InheritanceChain lists the active profiles from root to leaf.
	InheritanceChain []string
	// Command is the selected command path ("" for the application root).
	Command string
	// Values describes every value contributed by the chain.
	Values []ProfileValueExplanation
}

// explain builds the explanation for ctx using the resolver's view of the
// chain and the sources that won resolution (recorded on ctx).
func (r *profileResolver) explain(ctx *Context) *ProfileExplanation {
	selectedPath := ""
	if selected := ctx.Selected(); selected != nil {
		selectedPath = selected.Path()
	}
	explanation := &ProfileExplanation{
		Selected:         r.selected,
		DefaultSelected:  r.defaultSelected,
		InheritanceChain: append([]string{}, r.chainNames...),
		Command:          selectedPath,
		Values:           []ProfileValueExplanation{},
	}
	cliSet := map[*Flag]bool{}
	for _, path := range ctx.Path {
		if path.Flag != nil && !path.Resolved {
			cliSet[path.Flag] = true
		}
	}
	var walk func(*Node)
	walk = func(node *Node) {
		ownerPath := node.Path()
		ownerActive := pathHasPrefix(selectedPath, ownerPath)
		for _, flag := range node.Flags {
			entries := r.entriesFor(flag, ownerPath, selectedPath, ownerActive)
			winner := -1
			for i := range entries {
				if entries[i].active {
					winner = i
				}
			}
			for i := range entries {
				entry := entries[i]
				switch {
				case i == winner:
					entry.Effective = r.effectiveFor(flag, cliSet, ctx)
					if !entry.Effective {
						entry.OverriddenBy = r.overrideReason(flag, cliSet, ctx)
					}
				case !entry.Active:
					entry.OverriddenBy = inactiveReason(ownerPath, entry.Scope, selectedPath)
				default:
					entry.OverriddenBy = overriddenByEntry(entries[winner])
				}
				explanation.Values = append(explanation.Values, entry.ProfileValueExplanation)
			}
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(ctx.Model.Node)
	return explanation
}

type profileEntry struct {
	ProfileValueExplanation
	active bool
}

func (r *profileResolver) entriesFor(flag *Flag, ownerPath, selectedPath string, ownerActive bool) []profileEntry {
	entries := []profileEntry{}
	for _, layer := range r.chain {
		if raw, ok := lookupConfigValue(layer.global, flag); ok {
			entries = append(entries, profileEntry{
				active: ownerActive,
				ProfileValueExplanation: ProfileValueExplanation{
					Flag:    flag.Name,
					Value:   raw,
					Profile: layer.name,
					Scope:   "",
					Command: ownerPath,
					Active:  ownerActive,
				},
			})
		}
		scopes := append([]string{}, layer.scopeLabels...)
		sort.Slice(scopes, func(a, b int) bool {
			return len(pathSegments(scopes[a])) < len(pathSegments(scopes[b]))
		})
		for _, scope := range scopes {
			if !pathHasPrefix(scope, ownerPath) {
				continue
			}
			raw, ok := lookupConfigValue(layer.scopes[scope], flag)
			if !ok {
				continue
			}
			active := ownerActive && pathHasPrefix(selectedPath, scope)
			entries = append(entries, profileEntry{
				active: active,
				ProfileValueExplanation: ProfileValueExplanation{
					Flag:    flag.Name,
					Value:   raw,
					Profile: layer.name,
					Scope:   scope,
					Command: scope,
					Active:  active,
				},
			})
		}
	}
	return entries
}

func (r *profileResolver) effectiveFor(flag *Flag, cliSet map[*Flag]bool, ctx *Context) bool {
	if cliSet[flag] {
		return false
	}
	switch ctx.resolverSources[flag] {
	case sourceConfig, sourceGlobal, sourceContext:
		return false
	default:
		return true
	}
}

func (r *profileResolver) overrideReason(flag *Flag, cliSet map[*Flag]bool, ctx *Context) string {
	if cliSet[flag] {
		return fmt.Sprintf("command-line argument --%s", flag.Name)
	}
	switch ctx.resolverSources[flag] {
	case sourceConfig:
		return "configuration file"
	case sourceGlobal:
		return "registered resolver"
	case sourceContext:
		return "context resolver"
	default:
		return "a higher priority source"
	}
}

func overriddenByEntry(winner profileEntry) string {
	return fmt.Sprintf("profile %q%s", winner.Profile, scopeSuffix(winner.Scope))
}

func inactiveReason(ownerPath, scope, selectedPath string) string {
	switch {
	case scope != "":
		return fmt.Sprintf("command scope %q is not part of the selected command %q", scope, displayPath(selectedPath))
	case ownerPath != "":
		return fmt.Sprintf("owning command %q is not selected", ownerPath)
	default:
		return "not applicable to the selected command"
	}
}

func displayPath(path string) string {
	if path == "" {
		return "<root>"
	}
	return path
}

// profileConflictDetail annotates xor/and validation errors with the profile
// layers supplying the conflicting flags.
func (c *Context) profileConflictDetail() string {
	resolver := c.profile
	if resolver == nil || len(resolver.supplied) == 0 {
		return ""
	}
	lines := []string{}
	for _, path := range c.Path {
		for _, flag := range path.Flags {
			provision, ok := resolver.supplied[flag]
			if !ok || !flag.Set {
				continue
			}
			line := fmt.Sprintf("--%s is set by profile %q", flag.Name, resolver.chainNames[provision.layer])
			if provision.scope != "" {
				line += " for command " + strconvQuote(provision.scope)
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	header := "active profile chain: " + strings.Join(resolver.chainNames, " -> ")
	return header + "\n" + strings.Join(lines, "\n")
}

// ProfileExplanation returns the explanation of the selected profile chain for
// this parse, or nil if no profile was selected.
func (c *Context) ProfileExplanation() *ProfileExplanation {
	if c.profileExplanation != nil {
		return c.profileExplanation
	}
	resolver := c.profileState()
	if resolver == nil {
		return nil
	}
	c.profileExplanation = resolver.explain(c)
	return c.profileExplanation
}
