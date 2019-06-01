package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

var (
	out   = flag.String("o", "", "")
	pkg   = flag.String("pkg", "main", "")
	name  = flag.String("name", "Map", "")
	usage = `Usage: syncmap [options...] map[T1]T2

Options:
  -o         Specify file output. If none is specified, the name
             will be derived from the map type.
  -pkg       Package name to use in the generated code. If none is
             specified, the name will main.
  -name      Struct name to use in the generated code. If none is
             specified, the name will be Map.
`
)

func main() {
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, fmt.Sprintf(usage))
	}
	flag.Parse()
	g, err := NewGenerator()
	failOnErr(err)
	err = g.Mutate()
	failOnErr(err)
	err = g.Gen()
	failOnErr(err)
}

// Generator generates the typed syncmap object.
type Generator struct {
	// flag options.
	pkg   string // package name.
	out   string // file name.
	name  string // struct name.
	key   string // map key type.
	value string // map value type.
	// mutation state and traversal handlers.
	file   *ast.File
	fset   *token.FileSet
	funcs  map[string]func(*ast.FuncDecl)
	types  map[string]func(*ast.TypeSpec)
	values map[string]func(*ast.ValueSpec)
}

// NewGenerator returns a new generator for syncmap.
func NewGenerator() (g *Generator, err error) {
	defer catch(&err)
	g = &Generator{fset: token.NewFileSet(), pkg: *pkg, out: *out, name: *name}
	g.funcs = g.Funcs()
	g.types = g.Types()
	g.values = g.Values()
	exp, err := parser.ParseExpr(os.Args[len(os.Args)-1])
	check(err, "parse expr: %s", os.Args[len(os.Args)-1])
	m, ok := exp.(*ast.MapType)
	expect(ok, "invalid argument. expected map[T1]T2")
	b := bytes.NewBuffer(nil)
	err = format.Node(b, g.fset, m.Key)
	check(err, "format map key")
	g.key = b.String()
	b.Reset()
	err = format.Node(b, g.fset, m.Value)
	check(err, "format map value")
	g.value = b.String()
	if g.out == "" {
		g.out = strings.ToLower(g.name) + ".go"
	}
	return
}

// Mutate mutates the original `sync/map` AST and brings it to the desired state.
// It fails if it encounters an unrecognized node in the AST.
func (g *Generator) Mutate() (err error) {
	defer catch(&err)
	path := fmt.Sprintf("%s/src/sync/map.go", runtime.GOROOT())
	b, err := ioutil.ReadFile(path)
	check(err, "read %q file", path)
	f, err := parser.ParseFile(g.fset, "", b, parser.ParseComments)
	check(err, "parse %q file", path)
	f.Name.Name = g.pkg
	astutil.AddImport(g.fset, f, "sync")
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			handler, ok := g.funcs[d.Name.Name]
			expect(ok, "unrecognized function: %s", d.Name.Name)
			handler(d)
			delete(g.funcs, d.Name.Name)
		case *ast.GenDecl:
			switch d := d.Specs[0].(type) {
			case *ast.TypeSpec:
				handler, ok := g.types[d.Name.Name]
				expect(ok, "unrecognized type: %s", d.Name.Name)
				handler(d)
				delete(g.types, d.Name.Name)
			case *ast.ValueSpec:
				handler, ok := g.values[d.Names[0].Name]
				expect(ok, "unrecognized value: %s", d.Names[0].Name)
				handler(d)
				expect(len(d.Names) == 1, "mismatch values length: %d", len(d.Names))
				delete(g.values, d.Names[0].Name)
			}
		default:
			expect(false, "unrecognized type: %s", d)
		}
	}
	expect(len(g.funcs) == 0, "function was deleted")
	expect(len(g.types) == 0, "type was deleted")
	expect(len(g.values) == 0, "value was deleted")
	rename(f, map[string]string{
		"Map":      g.name,
		"entry":    "entry" + strings.Title(g.name),
		"readOnly": "readOnly" + strings.Title(g.name),
		"expunged": "expunged" + strings.Title(g.name),
		"newEntry": "newEntry" + strings.Title(g.name),
	})
	g.file = f
	return
}

// Gen dumps the mutated AST to a file in the configured destination.
func (g *Generator) Gen() (err error) {
	defer catch(&err)
	b := bytes.NewBuffer([]byte("// Code generated by syncmap; DO NOT EDIT.\n\n"))
	err = format.Node(b, g.fset, g.file)
	check(err, "format mutated code")
	err = ioutil.WriteFile(g.out, b.Bytes(), 0644)
	check(err, "writing file: %s", g.out)
	err = exec.Command("goimports", "-w", g.out).Run()
	check(err, "running goimports on: %s", g.out)
	return
}

// Values returns all ValueSpec handlers for AST mutation.
func (g *Generator) Values() map[string]func(*ast.ValueSpec) {
	return map[string]func(*ast.ValueSpec){
		"expunged": func(v *ast.ValueSpec) { g.replaceValue(v) },
	}
}

// Types returns all TypesSpec handlers for AST mutation.
func (g *Generator) Types() map[string]func(*ast.TypeSpec) {
	return map[string]func(*ast.TypeSpec){
		"Map": func(t *ast.TypeSpec) {
			l := t.Type.(*ast.StructType).Fields.List[0]
			l.Type = expr("sync.Mutex", l.Type.Pos())
			g.replaceKey(t.Type)
		},
		"readOnly": func(t *ast.TypeSpec) { g.replaceKey(t) },
		"entry":    func(*ast.TypeSpec) {},
	}
}

// Funcs returns all FuncDecl handlers for AST mutation.
func (g *Generator) Funcs() map[string]func(*ast.FuncDecl) {
	nop := func(*ast.FuncDecl) {}
	return map[string]func(*ast.FuncDecl){
		"Load": func(f *ast.FuncDecl) {
			g.replaceKey(f.Type.Params)
			g.replaceValue(f.Type.Results)
			renameNil(f.Body, f.Type.Results.List[0].Names[0].Name)
		},
		"load": func(f *ast.FuncDecl) {
			g.replaceValue(f)
			renameNil(f.Body, f.Type.Results.List[0].Names[0].Name)
		},
		"Store": func(f *ast.FuncDecl) {
			g.renameTuple(f.Type.Params)
		},
		"LoadOrStore": func(f *ast.FuncDecl) {
			g.renameTuple(f.Type.Params)
			g.replaceValue(f.Type.Results)
		},
		"tryLoadOrStore": func(f *ast.FuncDecl) {
			g.replaceValue(f)
			renameNil(f.Body, f.Type.Results.List[0].Names[0].Name)
		},
		"Range": func(f *ast.FuncDecl) {
			g.renameTuple(f.Type.Params.List[0].Type.(*ast.FuncType).Params)
		},
		"Delete":           func(f *ast.FuncDecl) { g.replaceKey(f) },
		"newEntry":         func(f *ast.FuncDecl) { g.replaceValue(f) },
		"tryStore":         func(f *ast.FuncDecl) { g.replaceValue(f) },
		"dirtyLocked":      func(f *ast.FuncDecl) { g.replaceKey(f) },
		"storeLocked":      func(f *ast.FuncDecl) { g.replaceValue(f) },
		"delete":           nop,
		"missLocked":       nop,
		"unexpungeLocked":  nop,
		"tryExpungeLocked": nop,
	}
}

// replaceKey replaces all `interface{}` occurrences in the given Node with the key node. 
func (g *Generator) replaceKey(n ast.Node) { replaceIface(n, g.key) }

// replaceValue replaces all `interface{}` occurrences in the given Node with the value node.
func (g *Generator) replaceValue(n ast.Node) { replaceIface(n, g.value) }

func (g *Generator) renameTuple(l *ast.FieldList) {
	if g.key == g.value {
		g.replaceKey(l.List[0])
		return
	}
	l.List = append(l.List, &ast.Field{
		Names: []*ast.Ident{l.List[0].Names[1]},
		Type:  l.List[0].Type,
	})
	l.List[0].Names = l.List[0].Names[:1]
	g.replaceKey(l.List[0])
	g.replaceValue(l.List[1])
}

func replaceIface(n ast.Node, s string) {
	astutil.Apply(n, func(c *astutil.Cursor) bool {
		n := c.Node()
		if it, ok := n.(*ast.InterfaceType); ok {
			c.Replace(expr(s, it.Interface))
		}
		return true
	}, nil)
}

func rename(f *ast.File, oldnew map[string]string) {
	astutil.Apply(f, func(c *astutil.Cursor) bool {
		switch n := c.Node().(type) {
		case *ast.Ident:
			if name, ok := oldnew[n.Name]; ok {
				n.Name = name
				n.Obj.Name = name
			}
		case *ast.FuncDecl:
			if name, ok := oldnew[n.Name.Name]; ok {
				n.Name.Name = name
			}
		}
		return true
	}, nil)
}

func renameNil(n ast.Node, name string) {
	astutil.Apply(n, func(c *astutil.Cursor) bool {
		if _, ok := c.Parent().(*ast.ReturnStmt); ok {
			if i, ok := c.Node().(*ast.Ident); ok && i.Name == new(types.Nil).String() {
				i.Name = name
			}
		}
		return true
	}, nil)
}

func expr(s string, pos token.Pos) ast.Expr {
	exp, err := parser.ParseExpr(s)
	check(err, "parse expr: %q", s)
	setPos(exp, pos)
	return exp
}

func setPos(n ast.Node, p token.Pos) {
	if reflect.ValueOf(n).IsNil() {
		return
	}
	switch n := n.(type) {
	case *ast.Ident:
		n.NamePos = p
	case *ast.MapType:
		n.Map = p
		setPos(n.Key, p)
		setPos(n.Value, p)
	case *ast.FieldList:
		n.Closing = p
		n.Opening = p
		if len(n.List) > 0 {
			setPos(n.List[0], p)
		}
	case *ast.Field:
		setPos(n.Type, p)
		if len(n.Names) > 0 {
			setPos(n.Names[0], p)
		}
	case *ast.FuncType:
		n.Func = p
		setPos(n.Params, p)
		setPos(n.Results, p)
	case *ast.ArrayType:
		n.Lbrack = p
		setPos(n.Elt, p)
	case *ast.StructType:
		n.Struct = p
		setPos(n.Fields, p)
	case *ast.SelectorExpr:
		setPos(n.X, p)
		n.Sel.NamePos = p
	case *ast.InterfaceType:
		n.Interface = p
		setPos(n.Methods, p)
	case *ast.StarExpr:
		n.Star = p
		setPos(n.X, p)
	default:
		panic(fmt.Sprintf("unknown type: %v", n))
	}
}

// check panics if the error is not nil.
func check(err error, msg string, args ...interface{}) {
	if err != nil {
		args = append(args, err)
		panic(genError{fmt.Sprintf(msg+": %s", args...)})
	}
}

// expect panic if the condition is false.
func expect(cond bool, msg string, args ...interface{}) {
	if !cond {
		panic(genError{fmt.Sprintf(msg, args...)})
	}
}

type genError struct {
	msg string
}

func (p genError) Error() string { return fmt.Sprintf("syncmap: %s", p.msg) }

func catch(err *error) {
	if e := recover(); e != nil {
		gerr, ok := e.(genError)
		if !ok {
			panic(e)
		}
		*err = gerr
	}
}

func failOnErr(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err.Error())
		os.Exit(1)
	}
}
