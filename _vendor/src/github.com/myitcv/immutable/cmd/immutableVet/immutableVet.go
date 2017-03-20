package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go/build"

	"github.com/kisielk/gotool"
	"github.com/myitcv/gogenerate"
	"github.com/myitcv/immutable"
)

const (
	skipFileComment = "//" + immutable.CmdImmutableVet + ":skipFile"
)

var fset = token.NewFileSet()

type immutableVetter struct {
	pkgs map[string]*ast.Package
	bpkg *build.Package

	wd string

	skipFiles map[string]bool

	info *types.Info

	// immTmpls is the set of immutable template types in the package
	// being analysed
	immTmpls map[types.Type]bool

	// helper field used to hold Range() method calls on immutable types
	rngs map[*ast.Ident]bool

	// valid composite literals
	vcls map[*ast.CompositeLit]bool

	errlist []immErr
}

var typesCache = map[string]bool{
	"time.Time": true,

	// TODO remove this hack
	"*time.Time": true,
}

type immErr struct {
	pos token.Position
	msg string
}

type errors []immErr

func main() {
	flag.Parse()

	defer func() {
		// if err, ok := recover().(error); ok {
		// 	log.Fatalln(err)
		// }
	}()

	wd, err := os.Getwd()
	if err != nil {
		fatalf("could not get the working directory")
	}

	specs := gotool.ImportPaths(flag.Args())

	emsgs := vet(wd, specs)

	for _, msg := range emsgs {
		fmt.Fprintf(os.Stderr, "%v\n", msg)
	}

	if len(emsgs) > 0 {
		os.Exit(1)
	}
}

func vet(wd string, specs []string) []immErr {
	var emsgs []immErr

	// vetting phase: vet all packages packages passed in through the command line
	for _, spec := range specs {

		// reuse spec and import paths map to depPkg
		bpkg, err := build.Import(spec, wd, 0)
		if err != nil {
			fatalf("unable to import %v relative to %v: %v", spec, wd, err)
		}

		iv := newImmutableVetter(bpkg, wd)

		emsgs = append(emsgs, iv.vetPackages()...)

	}

	for i := range ifaces {
		for n := range seenTypes {
			if types.Implements(n, i) && !isImmType(n) {
				emsgs = append(emsgs, immErr{
					msg: fmt.Sprintf("type %v which implements %v is not immutable", n, i),
					pos: fset.Position(n.Obj().Pos()),
				})
				continue
			}

			p := types.NewPointer(n)
			if types.Implements(p, i) && !isImmType(p) {
				emsgs = append(emsgs, immErr{
					msg: fmt.Sprintf("type %v which implements %v is not immutable", p, i),
					pos: fset.Position(n.Obj().Pos()),
				})
				continue
			}
		}
	}

	for i := range emsgs {
		rel, err := filepath.Rel(wd, emsgs[i].pos.Filename)
		if err != nil {
			fatalf("relative path error, %v", err)
		}

		emsgs[i].pos.Filename = rel
	}

	sort.Sort(errors(emsgs))

	return emsgs
}

func (iv *immutableVetter) ensurePointerTyp(n ast.Node, typ ast.Expr) {
	t := iv.info.Types[typ].Type
	p := types.NewPointer(t)
	switch immutable.IsImmType(p).(type) {
	case immutable.ImmTypeMap, immutable.ImmTypeSlice, immutable.ImmTypeStruct:
		iv.errorf(n.Pos(), "type should be %v", p)
	}
}

func (iv *immutableVetter) Visit(node ast.Node) ast.Visitor {

	switch node := node.(type) {
	case *ast.File:
		for _, cg := range node.Comments {
			for _, c := range cg.List {
				if c.Text == skipFileComment {
					iv.skipFiles[fset.Position(node.Pos()).Filename] = true
					return nil
				}
			}
		}
	case *ast.ValueSpec:
		iv.ensurePointerTyp(node, node.Type)
	case *ast.ArrayType:
		iv.ensurePointerTyp(node, node.Elt)
	case *ast.MapType:
		iv.ensurePointerTyp(node, node.Key)
		iv.ensurePointerTyp(node, node.Value)
	case *ast.Field:
		iv.ensurePointerTyp(node, node.Type)
	case *ast.UnaryExpr:
		if node.Op != token.AND {
			break
		}

		cl, ok := node.X.(*ast.CompositeLit)
		if !ok {
			break
		}

		t := iv.info.Types[cl.Type].Type
		p := types.NewPointer(t)
		switch immutable.IsImmType(p).(type) {
		case immutable.ImmTypeMap, immutable.ImmTypeSlice, immutable.ImmTypeStruct:
			iv.errorf(node.Pos(), "construct using new() or generated constructors")
			iv.vcls[cl] = true
		}
	case *ast.CompositeLit:
		if ok := iv.vcls[node]; ok {
			break
		}

		iv.ensurePointerTyp(node, node.Type)
	case *ast.TypeSpec:
		iv.ensurePointerTyp(node, node.Type)
	case *ast.SelectorExpr:
		sel, ok := iv.info.Selections[node]
		if !ok {
			// this is fine... !ok implies a selector expression
			// that is a qualified identifier as opposed to a method
			// field selector
			break
		}

		if !isImmListOrMap(sel.Recv()) {
			break
		}

		switch node.Sel.Name {
		case "Range":
			if _, ok := iv.rngs[node.Sel]; !ok {
				iv.rngs[node.Sel] = false
			}
		}
	case *ast.RangeStmt:
		v := node.X
		ce, ok := v.(*ast.CallExpr)
		if !ok {
			break
		}

		e := ce.Fun
		se, ok := e.(*ast.SelectorExpr)
		if !ok {
			break
		}

		sel, ok := iv.info.Selections[se]
		if !ok {
			fatalf("unable to type selection %v", se)
		}

		if !isImmListOrMap(sel.Recv()) {
			break
		}

		if sel.Kind() != types.MethodVal {
			break
		}

		ri := se.Sel
		if ri.Name != "Range" {
			break
		}
		iv.rngs[ri] = true
	case *ast.CallExpr:
		switch fun := node.Fun.(type) {
		case *ast.Ident:
			if fun.Name != "append" {
				break
			}

			if len(node.Args) != 2 {
				break
			}

			e := node.Args[1]
			ce, ok := e.(*ast.CallExpr)
			if !ok {
				break
			}

			se, ok := ce.Fun.(*ast.SelectorExpr)
			if !ok {
				break
			}

			sel, ok := iv.info.Selections[se]
			if !ok {
				break
			}

			if !isImmListOrMap(sel.Recv()) {
				break
			}

			ri := se.Sel
			if ri.Name != "Range" {
				break
			}

			if node.Ellipsis == node.Args[1].End() {
				iv.rngs[ri] = true
			}
		case *ast.SelectorExpr:
			sel, ok := iv.info.Selections[fun]
			if !ok {
				// this is fine... !ok implies a selector expression
				// that is a qualified identifier as opposed to a method
				// field selector
				break
			}

			if !isImmListOrMap(sel.Recv()) {
				break
			}

			if sel.Kind() != types.MethodVal {
				break
			}

			ri := fun.Sel
			if ri.Name != "Append" {
				break
			}

			if len(node.Args) != 1 {
				break
			}

			if node.Ellipsis == token.NoPos {
				break
			}

			ace, ok := node.Args[0].(*ast.CallExpr)
			if !ok {
				break
			}

			{
				se, ok := ace.Fun.(*ast.SelectorExpr)
				if !ok {
					break
				}

				sel, ok := iv.info.Selections[se]
				if !ok {
					// this is fine... !ok implies a selector expression
					// that is a qualified identifier as opposed to a method
					// field selector
					break
				}

				if !isImmListOrMap(sel.Recv()) {
					break
				}

				if sel.Kind() != types.MethodVal {
					break
				}

				ri := se.Sel
				if ri.Name == "Range" {
					iv.rngs[ri] = true
				}
			}
		}
	}
	return iv
}

func isImmListOrMap(t types.Type) bool {
	switch immutable.IsImmType(t).(type) {
	case immutable.ImmTypeMap, immutable.ImmTypeSlice:
		return true
	}

	return false
}

func newImmutableVetter(ipkg *build.Package, wd string) *immutableVetter {
	pkgs, err := parser.ParseDir(fset, ipkg.Dir, nil, parser.ParseComments)
	if err != nil {
		fatalf("could not parse package directory for %v", ipkg.Name)
	}

	return &immutableVetter{
		pkgs:      pkgs,
		bpkg:      ipkg,
		vcls:      make(map[*ast.CompositeLit]bool),
		wd:        wd,
		skipFiles: make(map[string]bool),
	}
}

func (iv *immutableVetter) isImmTmpl(t types.Type) bool {
	switch t := t.(type) {
	case *types.Pointer:
		return iv.isImmTmpl(t.Elem())
	}

	return iv.immTmpls[t]
}

func (iv *immutableVetter) vetPackages() []immErr {
	for _, pkg := range iv.pkgs {
		iv.rngs = make(map[*ast.Ident]bool)

		// make a list of files for using it in Check func
		files := make([]*ast.File, 0, len(pkg.Files))

		for _, f := range pkg.Files {
			files = append(files, f)
		}

		// check types for the core package
		conf := types.Config{
			Importer: importer.Default(),
		}
		info := &types.Info{
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
			Defs:       make(map[*ast.Ident]types.Object),
			Types:      make(map[ast.Expr]types.TypeAndValue),
			Implicits:  make(map[ast.Node]types.Object),
			Scopes:     make(map[ast.Node]*types.Scope),
		}
		_, err := conf.Check(iv.bpkg.ImportPath, fset, files, info)
		if err != nil {
			fatalf("type checking failed, %v", err)
		}
		iv.info = info

		iv.immTmpls = make(map[types.Type]bool)

		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok {
					continue
				}

				if gd.Tok != token.TYPE {
					continue
				}

				for _, s := range gd.Specs {
					ts := s.(*ast.TypeSpec)

					_, ok := immutable.IsImmTmpl(ts)
					if !ok {
						continue
					}

					o := info.ObjectOf(ts.Name)
					iv.immTmpls[o.Type()] = true

					st, ok := o.Type().(*types.Named).Underlying().(*types.Struct)
					if !ok {
						continue
					}

					for i := 0; i < st.NumFields(); i++ {
						f := st.Field(i)
						if !isImmType(f.Type()) {
							msg := fmt.Sprintf("immutable struct field must be immutable type; %v is not", f.Type())
							iv.errorf(f.Pos(), msg)
						}
					}
				}
			}
		}

		ast.Walk(iv, pkg)

		for exp, t := range info.Types {
			if !t.IsType() {
				continue
			}

			typ := t.Type

			if !iv.isImmTmpl(typ) {
				continue
			}

			fn := fset.Position(exp.Pos()).Filename

			if !gogenerate.FileGeneratedBy(fn, immutable.CmdImmutableGen) {
				iv.errorf(exp.Pos(), "template type %v should never get used", typ)
			}
		}

		// find selector exprs which access properties of Immutable types
		for exp, sel := range info.Selections {
			isField := sel.Kind() == types.FieldVal
			if !isField {
				continue
			}

			if immutable.IsImmType(sel.Recv()) == nil {
				continue
			}

			if iv.skipFiles[fset.Position(exp.X.Pos()).Filename] {
				continue
			}

			oname := sel.Obj().Name()
			iv.errorf(exp.X.Pos(), "should not be using %v of %v immutable type", oname, sel.Recv())
		}

		for k, v := range iv.rngs {
			if v == false {
				msg := fmt.Sprintf("Range() of immutable type must appear in a range statement or used with an ellipsis as the second argument to append")
				iv.errorf(k.NamePos, msg)
			}
		}
	}

	return iv.errlist
}

var ifaces = make(map[*types.Interface]bool)
var seenTypes = make(map[*types.Named]bool)

func isImmType(t types.Type) bool {
	if v, ok := typesCache[t.String()]; ok {
		return v
	}

	switch t := t.(type) {
	case *types.Named:

		typesCache[t.String()] = true
		seenTypes[t] = true

		v := isImmType(t.Underlying())
		typesCache[t.String()] = v

		return v
	case *types.Basic:
		return true
	case *types.Map, *types.Slice:
		return false
	case *types.Pointer:
		return immutable.IsImmType(t) != nil
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			f := t.Field(i)
			if !isImmType(f.Type()) {
				return false
			}
		}

		return true
	case *types.Interface:
		ifaces[t] = true
		return true
	case *types.Signature:
		return false
	default:
		fatalf("unable to handle type %T %v", t, t)
	}

	return false
}

func (iv *immutableVetter) errorf(pos token.Pos, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	iv.errlist = append(iv.errlist, immErr{
		pos: fset.Position(pos),
		msg: msg,
	})
}

func fatalf(format string, args ...interface{}) {
	panic(fmt.Errorf(format, args...))
}

func (r immErr) String() string {
	return fmt.Sprintf("%v:%v:%v: %v", r.pos.Filename, r.pos.Line, r.pos.Column, r.msg)
}

func (e errors) Len() int {
	return len(e)
}

func (e errors) Swap(i, j int) {
	e[i], e[j] = e[j], e[i]
}

func (e errors) Less(i, j int) bool {
	l, r := e[i].pos, e[j].pos

	if v := strings.Compare(l.Filename, r.Filename); v != 0 {
		return v < 0
	}

	if l.Line != r.Line {
		return l.Line < r.Line
	}

	return l.Column < r.Column
}