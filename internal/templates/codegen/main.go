package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/pkg/errors"
	"golang.stackrox.io/kube-linter/internal/check"
	"golang.stackrox.io/kube-linter/internal/set"
	"golang.stackrox.io/kube-linter/internal/stringutils"
	"golang.stackrox.io/kube-linter/internal/utils"
	"k8s.io/gengo/parser"
	"k8s.io/gengo/types"
)

var (
	knownNonTemplateDirs = set.NewFrozenStringSet("all", "codegen", "util")
)

const (
	metadataMarker = "+"

	paramsStructName = "Params"
)

type templateElem struct {
	ParamDesc check.ParameterDesc
	ParamJSON string
}

const (
	fileTemplateStr = `// Code generated by kube-linter template codegen. DO NOT EDIT.
// +build !templatecodegen

package params

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"golang.stackrox.io/kube-linter/internal/check"
	"golang.stackrox.io/kube-linter/internal/templates/util"
)

var (
	// Use some imports in case they don't get used otherwise.
	_ = util.MustParseParameterDesc
	_ = fmt.Sprintf

{{- range . }}

	{{ .ParamDesc.Name}}ParamDesc = util.MustParseParameterDesc({{backtick}}
{{- .ParamJSON -}}
{{backtick}})
{{- end }}

	ParamDescs = []check.ParameterDesc{
		{{- range . }}
		{{ .ParamDesc.Name}}ParamDesc,
		{{- end }}
	}
)

func (p *Params) Validate() error {
	var validationErrors []string
	{{- range . }}
	{{- if eq .ParamDesc.Type "object" }} 
	return errors.Errorf("parameter validation not yet supported for object type \"{{ .ParamDesc.Key }}\"")
	{{- end }}
	{{- if .ParamDesc.Required }}
	{{- if ne .ParamDesc.Type "string" }}
	return errors.Errorf("required parameter validation is currently only supported for strings, but {{ .ParamDesc.Key }} is not")
	{{- end }}
	if p.{{ .ParamDesc.XXXStructFieldName }} == "" {
		validationErrors = append(validationErrors, "required param {{.ParamDesc.Name}} not found")
	}
	{{- end }}
	{{- if .ParamDesc.Enum }}
	var found bool
	for _, allowedValue := range []string{
		{{- range .ParamDesc.Enum }}
		"{{ . }}",
		{{- end }}
	}{
		if p.{{ .ParamDesc.XXXStructFieldName }} == allowedValue {
			found = true
			break
		}
	}
	if !found {
		validationErrors = append(validationErrors, fmt.Sprintf("param {{ .ParamDesc.Name }} has invalid value %q, must be one of {{ .ParamDesc.Enum }}", p.{{ .ParamDesc.XXXStructFieldName }}))
	}
	{{- end }}
	{{- end }}
	if len(validationErrors) > 0 {
		return errors.Errorf("invalid parameters: %s", strings.Join(validationErrors, ", "))
    }
	return nil
}

// ParseAndValidate instantiates a Params object out of the passed map[string]interface{},
// validates it, and returns it.
// The return type is interface{} to satisfy the type in the Template struct.
func ParseAndValidate(m map[string]interface{}) (interface{}, error) {
	var p Params
	if err := util.DecodeMapStructure(m, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// WrapInstantiateFunc is a convenience wrapper that wraps an untyped instantiate function
// into a typed one.
func WrapInstantiateFunc(f func(p Params) (check.Func, error)) func (interface{}) (check.Func, error) {
	return func(paramsInt interface{}) (check.Func, error) {
		return f(paramsInt.(Params))
	}
}
`
)

var (
	fileTemplate = template.Must(template.New("gen").Funcs(sprig.TxtFuncMap()).Funcs(template.FuncMap{
		"backtick": func() string {
			return "`"
		},
	}).Parse(fileTemplateStr))
)

func lowerCaseFirstLetter(s string) string {
	return strings.ToLower(s[:1]) + s[1:]
}

func getName(member types.Member) string {
	if jsonTag := reflect.StructTag(member.Tags).Get("json"); jsonTag != "" {
		name, _ := stringutils.Split2(jsonTag, ",")
		if name != "" {
			return name
		}
	}
	return lowerCaseFirstLetter(member.Name)
}

func getDescription(member types.Member) string {
	firstCommentLineWithMetadata := len(member.CommentLines)
	for i, commentLine := range member.CommentLines {
		if strings.HasPrefix(commentLine, metadataMarker) {
			firstCommentLineWithMetadata = i
			break
		}
	}
	return strings.Join(member.CommentLines[:firstCommentLineWithMetadata], " ")
}

func setBoolBasedOnPresenceOfTag(valToSet *bool, tag string, extractedTags map[string][]string) error {
	if val, exists := extractedTags[tag]; exists {
		if len(val) > 1 || (len(val) == 0 && val[0] != "") {
			return errors.Errorf("invalid value for tag %s: %v; tag is only supported WITHOUT values", tag, val)
		}
		*valToSet = true
	}
	return nil
}

func constructParameterDescsFromStruct(typeSpec *types.Type) ([]check.ParameterDesc, error) {
	var paramDescs []check.ParameterDesc
	for _, member := range typeSpec.Members {
		if member.Embedded {
			return nil, errors.Errorf("cannot handle embedded member %s in %+v", member.Name, typeSpec)
		}

		desc := check.ParameterDesc{
			Name:               getName(member),
			Description:        getDescription(member),
			XXXStructFieldName: member.Name,
		}
		relevantTyp := member.Type
		if relevantTyp.Kind == types.Pointer {
			desc.XXXIsPointer = true
			relevantTyp = relevantTyp.Elem
		}
		switch kind := relevantTyp.Kind; kind {
		case types.Builtin:
			checkType, err := getCheckTypeFromParsedBuiltinType(relevantTyp)
			if err != nil {
				return nil, errors.Wrapf(err, "handling field %v", member.Name)
			}
			desc.Type = checkType
		case types.Slice:
			desc.Type = check.ArrayType
			// For now we only support array of builtin types. No array of objects or array of arrays.
			elemType, err := getCheckTypeFromParsedBuiltinType(member.Type.Elem)
			if err != nil {
				return nil, errors.Wrapf(err, "handling array elem type %v", member.Type.Elem)
			}
			desc.ArrayElemType = elemType
		case types.Struct:
			desc.Type = check.ObjectType
			subParams, err := constructParameterDescsFromStruct(member.Type)
			if err != nil {
				return nil, errors.Wrapf(err, "handling field %v", member.Name)
			}
			desc.SubParameters = subParams
		default:
			return nil, errors.Errorf("currently unsupported type %v", member.Type)
		}

		extractedTags := types.ExtractCommentTags(metadataMarker, member.CommentLines)
		desc.Examples = extractedTags["example"]
		desc.Enum = extractedTags["enum"]
		if err := setBoolBasedOnPresenceOfTag(&desc.Required, "required", extractedTags); err != nil {
			return nil, err
		}
		if err := setBoolBasedOnPresenceOfTag(&desc.NoRegex, "noregex", extractedTags); err != nil {
			return nil, err
		}
		if err := setBoolBasedOnPresenceOfTag(&desc.NotNegatable, "notnegatable", extractedTags); err != nil {
			return nil, err
		}
		paramDescs = append(paramDescs, desc)
	}
	return paramDescs, nil
}

func getCheckTypeFromParsedBuiltinType(typeSpec * types.Type) (check.ParameterType, error) {
	switch typeSpec {
	case types.String:
		return check.StringType, nil
	case types.Int:
		return check.IntegerType, nil
	case types.Float32, types.Float64:
		return check.NumberType, nil
	case types.Bool:
		return check.BooleanType, nil
	default:
		return "",  errors.Errorf("currently unsupported type %v", typeSpec)
	}
}

func processTemplate(dir string) error {
	b := parser.New()
	// This avoids parsing generated files in the package (since we add +build !templatecodegen to them,
	// which makes the parsing much quicker since the parser doesn't have to load any imported packages).
	b.AddBuildTags("templatecodegen")
	if err := b.AddDir(fmt.Sprintf("./%s/internal/params", dir)); err != nil {
		return err
	}
	typeUniverse, err := b.FindTypes()
	if err != nil {
		return err
	}
	pkgNames := b.FindPackages()
	if len(pkgNames) != 1 {
		return errors.Errorf("found unexpected number of packages in %+v: %d", pkgNames, len(pkgNames))
	}

	pkg := typeUniverse.Package(pkgNames[0])
	paramsType := pkg.Type(paramsStructName)

	if paramsType.Kind != types.Struct {
		return errors.Errorf("unexpected param type: %+v", paramsType)
	}
	paramDescs, err := constructParameterDescsFromStruct(paramsType)
	if err != nil {
		return err
	}

	var templateObj []templateElem

	for _, paramDesc := range paramDescs {
		buf := bytes.NewBuffer(nil)
		enc := json.NewEncoder(buf)
		enc.SetIndent("", "\t")
		if err := enc.Encode(paramDesc); err != nil {
			return errors.Wrapf(err, "couldn't marshal param %v", paramDesc)
		}

		templateObj = append(templateObj, templateElem{
			ParamDesc: paramDesc,
			ParamJSON: buf.String(),
		})
	}

	outFileName := filepath.Join(dir, "internal", "params", "gen-params.go")
	outF, err := os.Create(outFileName)
	if err != nil {
		return errors.Wrap(err, "creating output file")
	}
	defer utils.IgnoreError(outF.Close)
	if err := fileTemplate.Execute(outF, templateObj); err != nil {
		return err
	}
	return nil
}

func mainCmd() error {
	fileInfos, err := ioutil.ReadDir(".")
	if err != nil {
		return err
	}
	for _, fileInfo := range fileInfos {
		if !fileInfo.IsDir() {
			continue
		}
		if knownNonTemplateDirs.Contains(fileInfo.Name()) {
			continue
		}
		if err := processTemplate(fileInfo.Name()); err != nil {
			return errors.Wrapf(err, "processing dir %v", fileInfo.Name())
		}
	}
	return nil
}

func main() {
	if err := mainCmd(); err != nil {
		fmt.Printf("Error executing command: %v", err)
		os.Exit(1)
	}

}
