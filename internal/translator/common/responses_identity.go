package common

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// responsesToolKind identifies the Responses tool family represented by a
// provider-neutral declaration.
type responsesToolKind string

const (
	responsesFunctionTool responsesToolKind = "function"
	ResponsesCustomTool   responsesToolKind = "custom"
)

// ResponsesToolDeclaration is a tool declaration after namespace expansion.
// QualifiedName is the provider-facing name, while Name and Namespace retain
// the reversible Responses identity.
type ResponsesToolDeclaration struct {
	Kind           responsesToolKind
	Name           string
	Namespace      string
	QualifiedName  string
	Raw            string
	SourcePriority int
	Direct         bool
	Order          int
}

// ResponsesToolIdentity is a request-local registry of the tool declarations
// that survive namespace expansion and collision resolution.
type ResponsesToolIdentity struct {
	declarations []ResponsesToolDeclaration
	winners      map[string]ResponsesToolDeclaration
	aliases      map[string]string
}

// NewResponsesToolIdentity builds a deterministic identity registry from the
// top-level tools and Responses Lite additional_tools declarations.
func NewResponsesToolIdentity(requestRawJSON []byte) *ResponsesToolIdentity {
	identity := &ResponsesToolIdentity{
		winners: make(map[string]ResponsesToolDeclaration),
		aliases: make(map[string]string),
	}
	if len(requestRawJSON) == 0 || !gjson.ValidBytes(requestRawJSON) {
		return identity
	}

	root := gjson.ParseBytes(requestRawJSON)
	identity.collect(root.Get("tools"), 0, "", true)
	if input := root.Get("input"); input.IsArray() {
		for _, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
				identity.collect(item.Get("tools"), 1, "", true)
			}
		}
	}
	identity.selectWinners()
	return identity
}

// Declarations returns the winning declarations in their original canonical
// order. The returned slice is detached from the registry.
func (identity *ResponsesToolIdentity) Declarations() []ResponsesToolDeclaration {
	if identity == nil || len(identity.declarations) == 0 {
		return nil
	}
	declarations := make([]ResponsesToolDeclaration, 0, len(identity.winners))
	for _, declaration := range identity.declarations {
		winner, ok := identity.winners[declaration.QualifiedName]
		if ok && winner.Order == declaration.Order {
			declarations = append(declarations, declaration)
		}
	}
	return declarations
}

// resolve maps a provider-facing qualified name back to its Responses identity.
func (identity *ResponsesToolIdentity) resolve(qualifiedName string) (ResponsesToolDeclaration, bool) {
	if identity == nil {
		return ResponsesToolDeclaration{}, false
	}
	declaration, ok := identity.winners[strings.TrimSpace(qualifiedName)]
	return declaration, ok
}

// ResolveName maps a Responses name/namespace pair or a unique local alias to
// the provider-facing qualified name used by the winning declaration.
func (identity *ResponsesToolIdentity) ResolveName(name, namespace string) string {
	name = strings.TrimSpace(name)
	namespace = strings.TrimSpace(namespace)
	if name == "" {
		return ""
	}
	if namespace != "" {
		qualifiedName := qualifyResponsesToolName(namespace, name)
		if _, ok := identity.resolve(qualifiedName); ok {
			return qualifiedName
		}
		return qualifiedName
	}
	if _, ok := identity.resolve(name); ok {
		return name
	}
	if identity != nil {
		if qualifiedName := identity.aliases[name]; qualifiedName != "" {
			return qualifiedName
		}
	}
	return name
}

// IsCustom reports whether the winning declaration for a provider-facing name
// is a Responses custom tool.
func (identity *ResponsesToolIdentity) IsCustom(qualifiedName string) bool {
	declaration, ok := identity.resolve(qualifiedName)
	return ok && declaration.Kind == ResponsesCustomTool
}

// SingleCustomName returns the only emitted tool name when it is custom.
func (identity *ResponsesToolIdentity) SingleCustomName() (string, bool) {
	declarations := identity.Declarations()
	if len(declarations) != 1 || declarations[0].Kind != ResponsesCustomTool {
		return "", false
	}
	return declarations[0].QualifiedName, true
}

// SetToolCallIdentity writes the reversible Responses name and namespace for a
// provider-facing tool name. Unknown names remain unchanged and flat.
func (identity *ResponsesToolIdentity) SetToolCallIdentity(item []byte, qualifiedName, itemPath string) []byte {
	name := strings.TrimSpace(qualifiedName)
	namespace := ""
	if declaration, ok := identity.resolve(name); ok {
		name = declaration.Name
		namespace = declaration.Namespace
	}
	return setResponsesToolCallIdentity(item, name, namespace, itemPath)
}

func (identity *ResponsesToolIdentity) collect(tools gjson.Result, sourcePriority int, namespace string, direct bool) {
	if !tools.IsArray() {
		return
	}
	for _, tool := range tools.Array() {
		toolType := strings.TrimSpace(tool.Get("type").String())
		if toolType == "namespace" {
			childNamespace := joinResponsesNamespace(namespace, tool.Get("name").String())
			if childNamespace != "" {
				identity.collect(tool.Get("tools"), sourcePriority, childNamespace, false)
			}
			continue
		}

		var kind responsesToolKind
		switch toolType {
		case "", "function":
			kind = responsesFunctionTool
		case "custom":
			kind = ResponsesCustomTool
		default:
			continue
		}
		name := responsesDeclarationName(tool)
		if name == "" {
			continue
		}
		identity.declarations = append(
			identity.declarations, ResponsesToolDeclaration{
				Kind:           kind,
				Name:           name,
				Namespace:      namespace,
				QualifiedName:  qualifyResponsesToolName(namespace, name),
				Raw:            tool.Raw,
				SourcePriority: sourcePriority,
				Direct:         direct && namespace == "",
				Order:          len(identity.declarations),
			},
		)
	}
}

func (identity *ResponsesToolIdentity) selectWinners() {
	for _, declaration := range identity.declarations {
		current, exists := identity.winners[declaration.QualifiedName]
		if !exists || responsesDeclarationPrecedes(declaration, current) {
			identity.winners[declaration.QualifiedName] = declaration
		}
	}

	ambiguousAliases := make(map[string]struct{})
	for qualifiedName, declaration := range identity.winners {
		if declaration.Direct || declaration.Name == qualifiedName {
			identity.aliases[declaration.Name] = qualifiedName
			continue
		}
		if existing, exists := identity.aliases[declaration.Name]; exists && existing != qualifiedName {
			delete(identity.aliases, declaration.Name)
			ambiguousAliases[declaration.Name] = struct{}{}
			continue
		}
		if _, ambiguous := ambiguousAliases[declaration.Name]; !ambiguous {
			identity.aliases[declaration.Name] = qualifiedName
		}
	}
}

func responsesDeclarationPrecedes(left, right ResponsesToolDeclaration) bool {
	if left.SourcePriority != right.SourcePriority {
		return left.SourcePriority < right.SourcePriority
	}
	if left.Direct != right.Direct {
		return left.Direct
	}
	return left.Order < right.Order
}

func responsesDeclarationName(tool gjson.Result) string {
	if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
		return name
	}
	return strings.TrimSpace(tool.Get("function.name").String())
}

func joinResponsesNamespace(parent, child string) string {
	parent = strings.TrimSpace(strings.TrimSuffix(parent, "__"))
	child = strings.TrimSpace(strings.TrimSuffix(child, "__"))
	if parent == "" {
		return child
	}
	if child == "" {
		return parent
	}
	if child == parent || strings.HasPrefix(child, parent+"__") {
		return child
	}
	return parent + "__" + child
}

// qualifyResponsesToolName returns the stable flattened provider-facing name
// for a Responses namespace child.
func qualifyResponsesToolName(namespace, name string) string {
	name = strings.TrimSpace(name)
	namespace = strings.TrimSpace(strings.TrimSuffix(namespace, "__"))
	if name == "" || namespace == "" {
		return name
	}
	if name == namespace || strings.HasPrefix(name, namespace+"__") {
		return name
	}
	return namespace + "__" + name
}

// setResponsesToolCallIdentity writes a resolved Responses tool name and namespace.
func setResponsesToolCallIdentity(item []byte, name, namespace, itemPath string) []byte {
	namePath := "name"
	namespacePath := "namespace"
	if itemPath != "" {
		namePath = itemPath + ".name"
		namespacePath = itemPath + ".namespace"
	}
	item, _ = sjson.SetBytes(item, namePath, name)
	if namespace != "" {
		item, _ = sjson.SetBytes(item, namespacePath, namespace)
	} else {
		item, _ = sjson.DeleteBytes(item, namespacePath)
	}
	return item
}

// WrapResponsesCustomToolInput maps freeform input to the object shape expected
// by function-based provider protocols.
func WrapResponsesCustomToolInput(input gjson.Result) string {
	wrapped := []byte(`{"input":""}`)
	if input.Type == gjson.String {
		wrapped, _ = sjson.SetBytes(wrapped, "input", input.String())
	} else if input.Exists() && input.Raw != "" {
		wrapped, _ = sjson.SetRawBytes(wrapped, "input", []byte(input.Raw))
	}
	return string(wrapped)
}

// ParseResponsesCustomToolInput extracts freeform input from a complete
// function-style argument object. It rejects partial JSON so streaming callers
// can defer deltas until the wrapper is reversible.
func ParseResponsesCustomToolInput(arguments string) (string, bool) {
	if !gjson.Valid(arguments) {
		return "", false
	}
	value := gjson.Get(arguments, "input")
	if !value.Exists() {
		return "", false
	}
	if value.Type == gjson.String {
		return value.String(), true
	}
	return value.Raw, true
}

// UnwrapResponsesCustomToolInput restores freeform input from a provider's
// function-style argument object.
func UnwrapResponsesCustomToolInput(arguments string) string {
	if input, ok := ParseResponsesCustomToolInput(arguments); ok {
		return input
	}
	return arguments
}

// ResponsesToolOutputText converts structured Responses tool output to the text
// form accepted by provider tool-result messages.
func ResponsesToolOutputText(output gjson.Result) string {
	if output.Type == gjson.String {
		return output.String()
	}
	if output.IsArray() {
		var builder strings.Builder
		for _, part := range output.Array() {
			if part.Type == gjson.String {
				builder.WriteString(part.String())
				continue
			}
			if text := part.Get("text"); text.Exists() {
				builder.WriteString(text.String())
			}
		}
		return builder.String()
	}
	if output.Exists() {
		return output.Raw
	}
	return ""
}
