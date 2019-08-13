package runner

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/gravitational/force"
	"github.com/gravitational/force/pkg/builder"
	"github.com/gravitational/force/pkg/git"
	"github.com/gravitational/force/pkg/github"
	"github.com/gravitational/force/pkg/kube"
	"github.com/gravitational/force/pkg/logging"

	"github.com/gravitational/trace"
)

// NewGroup does nothing for now, used to logically group
// processes together
func NewGroup(runner *Runner) func(vars ...interface{}) force.Group {
	return func(vars ...interface{}) force.Group {
		return runner
	}
}

// Parse returns a new instance of runner
// using G file input
func Parse(inputs []string, runner *Runner) error {
	g := &gParser{
		runner: runner,
		functions: map[string]interface{}{
			// Standard library functions
			"Process":       runner.Process,
			"Sequence":      force.Sequence,
			"Continue":      force.Continue,
			"Parallel":      force.Parallel,
			"Files":         force.Files,
			"Shell":         force.Shell,
			"Oneshot":       force.Oneshot,
			"Exit":          force.Exit,
			"Env":           force.Env,
			"ExpectEnv":     force.ExpectEnv,
			"ID":            force.ID,
			"Var":           force.Var,
			"WithTempDir":   force.WithTempDir,
			"WithChangeDir": force.WithChangeDir,
			"Sprintf":       force.Sprintf,
			"Define":        force.Define,
			"Strings":       force.Strings,
			"Duplicate":     force.Duplicate,

			"Setup": NewGroup(runner),

			// Github functions
			"Github":       github.NewPlugin(runner),
			"PullRequests": github.NewWatch(runner),
			"PostStatus":   github.NewPostStatus(runner),
			"PostStatusOf": github.NewPostStatusOf(runner),

			// Git functions
			"Git":   git.NewPlugin(runner),
			"Clone": git.NewClone(runner),

			// Container Builder functions
			"Builder": builder.NewPlugin(runner),
			"Build":   builder.NewBuild(runner),
			"Push":    builder.NewPush(runner),
			"Prune":   builder.NewPrune(runner),

			// Log functions
			"Log": logging.NewPlugin(runner),

			// Kubernetes functions
			"Kube": kube.NewPluginFunc(runner),
			"Run":  kube.NewRun(runner),
		},
		getStruct: func(name string) (interface{}, error) {
			switch name {
			// Standard library structs
			case "Spec":
				return force.Spec{}, nil
				// Github structs
			case "GithubConfig":
				return github.Config{}, nil
			case "Source":
				return github.Source{}, nil
				// Git structs
			case "GitConfig":
				return git.Config{}, nil
			case "Repo":
				return git.Repo{}, nil
				// Container builder structs
			case "BuilderConfig":
				return builder.Config{}, nil
			case "Image":
				return builder.Image{}, nil
			case "Secret":
				return builder.Secret{}, nil
			case "Arg":
				return builder.Arg{}, nil
				// Log structs
			case "LogConfig":
				return logging.Config{}, nil
			case "Output":
				return logging.Output{}, nil
				// Kube structs
			case "KubeConfig":
				return kube.Config{}, nil
			case "Job":
				return kube.Job{}, nil
			case "Container":
				return kube.Container{}, nil
			case "SecurityContext":
				return kube.SecurityContext{}, nil
			case "EnvVar":
				return kube.EnvVar{}, nil
			case "Volume":
				return kube.Volume{}, nil
			case "VolumeMount":
				return kube.VolumeMount{}, nil
			case "EmptyDir":
				return kube.EmptyDir{}, nil
			default:
				return nil, trace.BadParameter("unsupported struct: %v", name)
			}
		},
	}
	for _, input := range inputs {
		expr, err := parser.ParseExpr(input)
		if err != nil {
			return trace.Wrap(err)
		}
		_, err = g.parseNode(expr)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	// if after parsing, logging plugin is not set up
	// set it up with default plugin instance
	_, ok := runner.GetVar(logging.LoggingPlugin)
	if !ok {
		runner.SetVar(logging.LoggingPlugin, &logging.Plugin{})
	}
	return nil
}

// gParser is a parser that parses G files
type gParser struct {
	runner    *Runner
	functions map[string]interface{}
	structs   map[string]interface{}
	getStruct func(name string) (interface{}, error)
}

func (g *gParser) parseNode(node ast.Node) (interface{}, error) {
	switch n := node.(type) {
	case *ast.BasicLit:
		literal, err := literalToValue(n)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return literal, nil
	case *ast.CallExpr:
		// We expect function that will return predicate
		name, err := getIdentifier(n.Fun)
		if err != nil {
			return nil, err
		}
		fn, err := g.getFunction(name)
		if err != nil {
			return nil, err
		}
		arguments, err := g.evaluateArguments(n.Args)
		if err != nil {
			return nil, err
		}
		return callFunction(fn, arguments)
	case *ast.ParenExpr:
		return g.parseNode(n.X)
	}
	return nil, trace.BadParameter("unsupported %T", node)
}

func (g *gParser) evaluateArguments(nodes []ast.Expr) ([]interface{}, error) {
	out := make([]interface{}, len(nodes))
	for i, n := range nodes {
		val, err := g.evaluateExpr(n)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		out[i] = val
	}
	return out, nil
}

func (g *gParser) evaluateStructFields(nodes []ast.Expr) (map[string]interface{}, error) {
	out := make(map[string]interface{}, len(nodes))
	for _, n := range nodes {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return nil, trace.BadParameter("expected key value expression, got %v", n)
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			return nil, trace.BadParameter("expected value identifier, got %#v", n)
		}
		val, err := g.evaluateExpr(kv.Value)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if _, exists := out[key.Name]; exists {
			return nil, trace.BadParameter("duplicate struct field %q", key.Name)
		}
		out[key.Name] = val
	}
	return out, nil
}

func (g *gParser) evaluateExpr(n ast.Expr) (interface{}, error) {
	switch l := n.(type) {
	case *ast.CompositeLit:
		switch literal := l.Type.(type) {
		case *ast.Ident:
			structProto, err := g.getStruct(literal.Name)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			fields, err := g.evaluateStructFields(l.Elts)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			st, err := createStruct(structProto, fields)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return st, nil
		case *ast.ArrayType:
			arrayType, ok := literal.Elt.(*ast.Ident)
			if !ok {
				return nil, trace.BadParameter("unsupported composite literal: %v %T", literal.Elt, literal.Elt)
			}
			structProto, err := g.getStruct(arrayType.Name)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			slice := reflect.MakeSlice(reflect.SliceOf(reflect.TypeOf(structProto)), len(l.Elts), len(l.Elts))
			for i, el := range l.Elts {
				member, ok := el.(*ast.CompositeLit)
				if !ok {
					return nil, trace.BadParameter("unsupported composite literal type: %T", l.Type)
				}
				fields, err := g.evaluateStructFields(member.Elts)
				if err != nil {
					return nil, trace.Wrap(err)
				}
				st, err := createStruct(structProto, fields)
				if err != nil {
					return nil, trace.Wrap(err)
				}
				v := slice.Index(i)
				v.Set(reflect.ValueOf(st))
			}
			return slice.Interface(), nil
		default:
			return nil, trace.BadParameter("unsupported composite literal: %v %T", l.Type, l.Type)
		}
	case *ast.BasicLit:
		val, err := literalToValue(l)
		if err != nil {
			return nil, err
		}
		return val, nil
	case *ast.Ident:
		if l.Name == "true" {
			return force.Bool(true), nil
		} else if l.Name == "false" {
			return force.Bool(false), nil
		}
		val, err := getIdentifier(l)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return val, nil
	case *ast.CallExpr:
		name, err := getIdentifier(l.Fun)
		if err != nil {
			return nil, err
		}
		fn, err := g.getFunction(name)
		if err != nil {
			return nil, err
		}
		arguments, err := g.evaluateArguments(l.Args)
		if err != nil {
			return nil, err
		}
		return callFunction(fn, arguments)
	case *ast.UnaryExpr:
		if l.Op != token.AND {
			return nil, trace.BadParameter("operator %v is not supported", l.Op)
		}
		expr, err := g.evaluateExpr(l.X)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if reflect.TypeOf(expr).Kind() != reflect.Struct {
			return nil, trace.BadParameter("don't know how to take address of %v", reflect.TypeOf(expr).Kind())
		}
		ptr := reflect.New(reflect.TypeOf(expr))
		ptr.Elem().Set(reflect.ValueOf(expr))
		return ptr.Interface(), nil
	default:
		return nil, trace.BadParameter("%T is not supported", n)
	}
}

func (g *gParser) getFunction(name string) (interface{}, error) {
	fn, exists := g.functions[name]
	if !exists {
		return nil, trace.BadParameter("unsupported function: %v", name)
	}
	return fn, nil
}

func getIdentifier(node ast.Node) (string, error) {
	sexpr, ok := node.(*ast.SelectorExpr)
	if ok {
		id, ok := sexpr.X.(*ast.Ident)
		if !ok {
			return "", trace.BadParameter("expected selector identifier, got: %T in %#v", sexpr.X, sexpr.X)
		}
		return fmt.Sprintf("%s.%s", id.Name, sexpr.Sel.Name), nil
	}
	id, ok := node.(*ast.Ident)
	if !ok {
		return "", trace.BadParameter("expected identifier, got: %T", node)
	}
	return id.Name, nil
}

func literalToValue(a *ast.BasicLit) (interface{}, error) {
	switch a.Kind {
	case token.FLOAT:
		value, err := strconv.ParseFloat(a.Value, 64)
		if err != nil {
			return nil, trace.BadParameter("failed to parse argument: %s, error: %s", a.Value, err)
		}
		return value, nil
	case token.INT:
		value, err := strconv.Atoi(a.Value)
		if err != nil {
			return nil, trace.BadParameter("failed to parse argument: %s, error: %s", a.Value, err)
		}
		return force.Int(value), nil
	case token.STRING:
		value, err := strconv.Unquote(a.Value)
		if err != nil {
			return nil, trace.BadParameter("failed to parse argument: %s, error: %s", a.Value, err)
		}
		return force.String(value), nil
	}
	return nil, trace.BadParameter("unsupported function argument type: '%v'", a.Kind)
}

func callFunction(f interface{}, args []interface{}) (v interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = trace.BadParameter("failed calling function %v %v", functionName(f), r)
		}
	}()
	arguments := make([]reflect.Value, len(args))
	for i, a := range args {
		arguments[i] = reflect.ValueOf(a)
	}
	fn := reflect.ValueOf(f)
	ret := fn.Call(arguments)
	switch len(ret) {
	case 1:
		return ret[0].Interface(), nil
	case 2:
		v, e := ret[0].Interface(), ret[1].Interface()
		if e == nil {
			return v, nil
		}
		err, ok := e.(error)
		if !ok {
			return nil, trace.BadParameter("expected error as a second return value, got %T", e)
		}
		return v, err
	}
	return nil, trace.BadParameter("expected at least one return argument for '%v'", fn)
}

func createStruct(val interface{}, args map[string]interface{}) (v interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = trace.BadParameter("struct %v: %v", reflect.TypeOf(val).Name(), r)
		}
	}()
	structType := reflect.TypeOf(val)
	st := reflect.New(structType)
	for key, val := range args {
		field := st.Elem().FieldByName(key)
		if !field.IsValid() {
			return nil, trace.BadParameter("field %q is not found in %v", key, structType.Name())
		}
		if !field.CanSet() {
			return nil, trace.BadParameter("can't set value of %v", field)
		}
		field.Set(reflect.ValueOf(val))
	}
	return st.Elem().Interface(), nil
}

func functionName(i interface{}) string {
	fullPath := runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
	return strings.TrimPrefix(filepath.Ext(fullPath), ".")
}
