package loader

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"html/template"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/golang/protobuf/proto"
	desc "github.com/golang/protobuf/protoc-gen-go/descriptor"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
	"google.golang.org/genproto/googleapis/api/annotations"
)

// Load loads the Gunk packages on the provided patterns, and generates the
// corresponding proto files. Similar to Go, if a path begins with ".", it is
// interpreted as a file system path where a package is located, and "..."
// patterns are supported.
func Load(patterns ...string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	// First, translate the patterns to package paths.
	cfg := &packages.Config{Mode: packages.LoadFiles}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return err
	}
	pkgPaths := make([]string, len(pkgs))
	for i, pkg := range pkgs {
		pkgPaths[i] = pkg.PkgPath
	}

	l, err := New(wd, pkgPaths...)
	if err != nil {
		return err
	}

	for _, path := range pkgPaths {
		if err := l.GeneratePkg(path); err != nil {
			return err
		}
	}

	return nil
}

// Loader
type Loader struct {
	wd string

	gfile *ast.File
	pfile *desc.FileDescriptorProto
	tpkg  *types.Package

	fset    *token.FileSet
	tconfig *types.Config
	info    *types.Info

	// Maps from package import path to package information.
	loadPkgs map[string]*packages.Package
	astPkgs  map[string]map[string]*ast.File
	typePkgs map[string]*types.Package

	toGen     map[string]map[string]bool
	allProto  map[string]*desc.FileDescriptorProto
	origPaths map[string]string

	msgIndex  int32
	srvIndex  int32
	enumIndex int32
}

// New creates a Gunk loader for the specified working directory.
func New(wd string, paths ...string) (*Loader, error) {
	l := &Loader{
		wd:   wd,
		fset: token.NewFileSet(),
		tconfig: &types.Config{
			DisableUnusedImportCheck: true,
		},
		info: &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		},

		loadPkgs:  make(map[string]*packages.Package),
		typePkgs:  make(map[string]*types.Package),
		astPkgs:   make(map[string]map[string]*ast.File),
		toGen:     make(map[string]map[string]bool),
		allProto:  make(map[string]*desc.FileDescriptorProto),
		origPaths: make(map[string]string),
	}
	l.tconfig.Importer = l

	for _, path := range paths {
		if err := l.addPkg(path); err != nil {
			return nil, err
		}
		if err := l.translatePkg(path); err != nil {
			return nil, err
		}
	}
	if err := l.loadProtoDeps(); err != nil {
		return nil, err
	}

	return l, nil
}

// GeneratePkg runs the proto files resulting from translating gunk packages
// through a code generator, such as protoc-gen-go to generate Go packages.
//
// Generated files are written to the same directory, next to the source gunk
// files.
func (l *Loader) GeneratePkg(path string) error {
	req := l.requestForPkg(path)
	bs, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	cmd := exec.Command("protoc-gen-go")
	cmd.Stdin = bytes.NewReader(bs)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("error executing 'protoc-gen-go': %s, %v", out, err)
	}
	var resp plugin.CodeGeneratorResponse
	if err := proto.Unmarshal(out, &resp); err != nil {
		return err
	}
	for _, rf := range resp.File {
		// to turn foo.gunk.pb.go into foo.pb.go
		inPath := strings.Replace(*rf.Name, ".pb.go", "", 1)
		outPath := l.origPaths[inPath]
		outPath = strings.Replace(outPath, ".gunk", ".pb.go", 1)
		data := []byte(*rf.Content)
		if err := ioutil.WriteFile(outPath, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loader) requestForPkg(path string) *plugin.CodeGeneratorRequest {
	// For deterministic output, as the first file in each package
	// gets an extra package godoc.
	req := &plugin.CodeGeneratorRequest{
		Parameter: proto.String("plugins=grpc"),
	}
	for file := range l.toGen[path] {
		req.FileToGenerate = append(req.FileToGenerate, file)
	}
	sort.Strings(req.FileToGenerate)
	for _, pfile := range l.allProto {
		req.ProtoFile = append(req.ProtoFile, pfile)
	}
	return req
}

// translatePkg translates all the gunk files in a gunk package to the
// proto language. All the files within the package, including all the
// files for its transitive dependencies, must already be loaded via
// addPkg.
func (l *Loader) translatePkg(path string) error {
	l.tpkg = l.typePkgs[path]
	astFiles := l.astPkgs[path]
	for file := range astFiles {
		if err := l.translateFile(path, file); err != nil {
			return err
		}
	}

	for name, gfile := range astFiles {
		pfile := l.allProto[name]
		for oname := range astFiles {
			if name != oname {
				pfile.Dependency = append(pfile.Dependency, oname)
			}
		}
		for _, imp := range gfile.Imports {
			if imp.Name != nil && imp.Name.Name == "_" {
				continue
			}
			opath, _ := strconv.Unquote(imp.Path.Value)
			for oname := range l.astPkgs[opath] {
				pfile.Dependency = append(pfile.Dependency, oname)
			}
		}
	}
	return nil
}

// translateFile translates a single gunk file to a proto file.
func (l *Loader) translateFile(path, file string) error {
	l.msgIndex = 0
	l.srvIndex = 0
	l.enumIndex = 0
	l.toGen[path][file] = true
	if _, ok := l.allProto[file]; ok {
		return nil
	}
	lpkg := l.loadPkgs[path]
	astFiles := l.astPkgs[path]
	l.gfile = astFiles[file]
	l.pfile = &desc.FileDescriptorProto{
		Syntax:  proto.String("proto3"),
		Name:    proto.String(file),
		Package: proto.String(lpkg.PkgPath),
		Options: &desc.FileOptions{
			GoPackage: proto.String(lpkg.Name),
		},
	}
	l.addDoc(l.gfile.Doc, nil, packagePath)
	for _, decl := range l.gfile.Decls {
		if err := l.translateDecl(decl); err != nil {
			return err
		}
	}
	l.allProto[file] = l.pfile
	return nil
}

// translateDecl translates a top-level declaration in a gunk file. It
// only acts on type declarations; struct types become proto messages,
// interfaces become services, and basic integer types become enums.
func (l *Loader) translateDecl(decl ast.Decl) error {
	gd, ok := decl.(*ast.GenDecl)
	if !ok {
		return fmt.Errorf("invalid declaration %T", decl)
	}
	switch gd.Tok {
	case token.TYPE:
		// continue below
	case token.CONST:
		return nil // used for enums
	case token.IMPORT:
		return nil // imports; ignore
	default:
		return fmt.Errorf("invalid declaration token %v", gd.Tok)
	}
	for _, spec := range gd.Specs {
		ts := spec.(*ast.TypeSpec)
		if ts.Doc == nil {
			// pass it on to the helpers
			ts.Doc = gd.Doc
		}
		switch ts.Type.(type) {
		case *ast.StructType:
			msg, err := l.protoMessage(ts)
			if err != nil {
				return err
			}
			l.pfile.MessageType = append(l.pfile.MessageType, msg)
		case *ast.InterfaceType:
			srv, err := l.protoService(ts)
			if err != nil {
				return err
			}
			l.pfile.Service = append(l.pfile.Service, srv)
		case *ast.Ident:
			enum, err := l.protoEnum(ts)
			if err != nil {
				return err
			}
			l.pfile.EnumType = append(l.pfile.EnumType, enum)
		default:
			return fmt.Errorf("invalid declaration type %T", ts.Type)
		}
	}
	return nil
}

func (l *Loader) addDoc(doc *ast.CommentGroup, f func(string) string, path ...int32) {
	if doc == nil {
		return
	}
	if l.pfile.SourceCodeInfo == nil {
		l.pfile.SourceCodeInfo = &desc.SourceCodeInfo{}
	}
	text := doc.Text()
	if f != nil {
		text = f(text)
	}
	l.pfile.SourceCodeInfo.Location = append(l.pfile.SourceCodeInfo.Location,
		&desc.SourceCodeInfo_Location{
			Path:            path,
			LeadingComments: proto.String(text),
		},
	)
}

func (l *Loader) protoMessage(tspec *ast.TypeSpec) (*desc.DescriptorProto, error) {
	l.addDoc(tspec.Doc, nil, messagePath, l.msgIndex)
	msg := &desc.DescriptorProto{
		Name: proto.String(tspec.Name.Name),
	}
	stype := tspec.Type.(*ast.StructType)
	for i, field := range stype.Fields.List {
		if len(field.Names) != 1 {
			return nil, fmt.Errorf("need all fields to have one name")
		}
		l.addDoc(field.Doc, nil, messagePath, l.msgIndex, messageFieldPath, int32(i))
		pfield := &desc.FieldDescriptorProto{
			Name:   proto.String(field.Names[0].Name),
			Number: protoNumber(field.Tag),
		}
		switch ptype, plabel, tname := l.protoType(field.Type, nil); ptype {
		case 0:
			return nil, fmt.Errorf("unsupported field type: (%T)%v", field.Type, field.Type)
		case desc.FieldDescriptorProto_TYPE_ENUM, desc.FieldDescriptorProto_TYPE_MESSAGE:
			pfield.Type = &ptype
			pfield.TypeName = &tname
			pfield.Label = &plabel
		default:
			pfield.Type = &ptype
			pfield.Label = &plabel
		}
		msg.Field = append(msg.Field, pfield)
	}
	l.msgIndex++
	return msg, nil
}

func (l *Loader) protoService(tspec *ast.TypeSpec) (*desc.ServiceDescriptorProto, error) {
	srv := &desc.ServiceDescriptorProto{
		Name: proto.String(tspec.Name.Name),
	}
	itype := tspec.Type.(*ast.InterfaceType)
	for i, method := range itype.Methods.List {
		if len(method.Names) != 1 {
			return nil, fmt.Errorf("need all methods to have one name")
		}
		tag := ""
		fn := func(text string) string {
			text, tag = splitGunkTag(text)
			return text
		}
		l.addDoc(method.Doc, fn, servicePath, l.srvIndex,
			serviceMethodPath, int32(i))
		pmethod := &desc.MethodDescriptorProto{
			Name: proto.String(method.Names[0].Name),
		}
		sign := method.Type.(*ast.FuncType)
		var err error
		pmethod.InputType, err = l.protoParamType(sign.Params)
		if err != nil {
			return nil, err
		}
		pmethod.OutputType, err = l.protoParamType(sign.Results)
		if err != nil {
			return nil, err
		}
		if tag != "" {
			edesc, val, err := l.interpretTagValue(tag)
			if err != nil {
				return nil, err
			}
			if pmethod.Options == nil {
				pmethod.Options = &desc.MethodOptions{}
			}
			// TODO: actually use the
			// protoc-gen-grpc-gateway to make this do
			// something
			if err := proto.SetExtension(pmethod.Options, edesc, val); err != nil {
				return nil, err
			}
		}
		srv.Method = append(srv.Method, pmethod)
	}
	l.srvIndex++
	return srv, nil
}

func (l *Loader) interpretTagValue(tag string) (*proto.ExtensionDesc, interface{}, error) {
	// use Eval to resolve the type, and check for any errors in the
	// value expression
	tv, err := types.Eval(l.fset, l.tpkg, l.gfile.End(), tag)
	if err != nil {
		return nil, nil, err
	}
	switch s := tv.Type.String(); s {
	case "github.com/gunk/opt/http.Match":
		// an error would be caught in Eval
		expr, _ := parser.ParseExpr(tag)
		rule := &annotations.HttpRule{}
		for _, elt := range expr.(*ast.CompositeLit).Elts {
			kv := elt.(*ast.KeyValueExpr)
			val, _ := strconv.Unquote(kv.Value.(*ast.BasicLit).Value)
			method := "GET"
			switch name := kv.Key.(*ast.Ident).Name; name {
			case "Method":
				method = val
			case "Path":
				switch method {
				case "GET":
					rule.Pattern = &annotations.HttpRule_Get{Get: val}
				case "POST":
					rule.Pattern = &annotations.HttpRule_Post{Post: val}
				}
			case "Body":
				rule.Body = val
			}
		}
		return annotations.E_Http, rule, nil
	default:
		return nil, nil, fmt.Errorf("unknown option type: %s", s)
	}
}

func (l *Loader) protoParamType(fields *ast.FieldList) (*string, error) {
	if fields == nil || len(fields.List) == 0 {
		l.addProtoDep("google/protobuf/empty.proto")
		return proto.String(".google.protobuf.Empty"), nil
	}
	if len(fields.List) > 1 {
		return nil, fmt.Errorf("need all methods to have <=1 results")
	}
	field := fields.List[0]
	_, _, tname := l.protoType(field.Type, nil)
	if tname == "" {
		return nil, fmt.Errorf("could not get type for %v", field.Type)
	}
	return &tname, nil
}

func (l *Loader) protoEnum(tspec *ast.TypeSpec) (*desc.EnumDescriptorProto, error) {
	l.addDoc(tspec.Doc, nil, enumPath, l.enumIndex)
	enum := &desc.EnumDescriptorProto{
		Name: proto.String(tspec.Name.Name),
	}
	enumType := l.info.TypeOf(tspec.Name)
	for _, decl := range l.gfile.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for i, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			// .proto files have the same limitation, and it
			// allows per-value godocs
			if len(vs.Names) != 1 {
				return nil, fmt.Errorf("need all value specs to define one name")
			}
			name := vs.Names[0]
			if l.info.TypeOf(name) != enumType {
				continue
			}
			// SomeVal will be exported as SomeType_SomeVal
			l.addDoc(vs.Doc, namePrefix(tspec.Name.Name),
				enumPath, l.enumIndex, enumValuePath, int32(i))
			val := l.info.Defs[name].(*types.Const).Val()
			ival, _ := constant.Int64Val(val)
			enum.Value = append(enum.Value, &desc.EnumValueDescriptorProto{
				Name:   proto.String(name.Name),
				Number: proto.Int32(int32(ival)),
			})
		}
	}
	l.enumIndex++
	return enum, nil
}

func (l *Loader) protoType(expr ast.Expr, pkg *types.Package) (desc.FieldDescriptorProto_Type, desc.FieldDescriptorProto_Label, string) {
	if pkg == nil {
		pkg = l.tpkg
	}
	switch x := expr.(type) {
	case *ast.Ident:
		// Map Go types to proto types
		// https://developers.google.com/protocol-buffers/docs/proto3#scalar
		switch x.Name {
		case "string":
			return desc.FieldDescriptorProto_TYPE_STRING, 0, x.Name
		case "int", "int32":
			return desc.FieldDescriptorProto_TYPE_INT32, 0, x.Name
		case "bool":
			return desc.FieldDescriptorProto_TYPE_BOOL, 0, x.Name
		default:
			fullName := "." + pkg.Path() + "." + x.Name
			// Check if the identifier is an already known 'message' or 'enum.
			// NOTE: This checks all of the parsed gunk packages for the type.
			// TODO: There is likely a much more robust way to handle this.
			for _, p := range l.astPkgs[pkg.Path()] {
				for _, decl := range p.Decls {
					gd, ok := decl.(*ast.GenDecl)
					if !ok {
						continue
					}
					for _, spec := range gd.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						switch ts.Type.(type) {
						case *ast.StructType:
							if ts.Name.Name == x.Name {
								return desc.FieldDescriptorProto_TYPE_MESSAGE, 0, fullName
							}
						case *ast.Ident:
							if ts.Name.Name == x.Name {
								return desc.FieldDescriptorProto_TYPE_ENUM, 0, fullName
							}
						}
					}
				}
			}
			return 0, 0, ""
		}
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			break
		}
		pkg := l.info.ObjectOf(id).(*types.PkgName).Imported()
		return l.protoType(x.Sel, pkg)
	case *ast.ArrayType:
		// Array's arent handled, only slices. Check if this is an array.
		if x.Len != nil {
			return 0, 0, ""
		}
		typ, _, name := l.protoType(x.Elt, pkg)
		if typ == 0 {
			return 0, 0, ""
		}
		return typ, desc.FieldDescriptorProto_LABEL_REPEATED, name
	}
	return 0, 0, ""
}

// addPkg sets up a gunk package to be translated and generated. It is
// parsed from the gunk files on disk and type-checked, gathering all
// the info needed later on.
func (l *Loader) addPkg(pkgPath string) error {
	// First, translate the patterns to package paths.
	cfg := &packages.Config{
		Mode: packages.LoadFiles,
	}
	pkgs, err := packages.Load(cfg, pkgPath)
	if err != nil {
		return err
	}
	if len(pkgs) != 1 {
		panic("expected go/packages.Load to return exactly one package")
	}
	lpkg := pkgs[0]
	if len(lpkg.Errors) > 0 {
		return lpkg.Errors[0]
	}

	pkgDir := ""
	for _, gofile := range lpkg.GoFiles {
		dir := filepath.Dir(gofile)
		if pkgDir == "" {
			pkgDir = dir
		} else if dir != pkgDir {
			return fmt.Errorf("multiple dirs for %s: %s %s",
				pkgPath, pkgDir, dir)
		}
	}

	matches, err := filepath.Glob(filepath.Join(pkgDir, "*.gunk"))
	if err != nil {
		return err
	}
	// TODO: support multiple packages
	var list []*ast.File
	pkgName := "default"
	astFiles := make(map[string]*ast.File)
	for _, match := range matches {
		file, err := parser.ParseFile(l.fset, match, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		pkgName = file.Name.Name
		// to make the generated code independent of the current
		// directory when running gunk
		relPath := pkgPath + "/" + filepath.Base(match)
		astFiles[relPath] = file
		l.origPaths[relPath] = match
		list = append(list, file)
	}
	tpkg := types.NewPackage(pkgPath, pkgName)
	check := types.NewChecker(l.tconfig, l.fset, tpkg, l.info)
	if err := check.Files(list); err != nil {
		return err
	}
	l.loadPkgs[pkgPath] = lpkg
	l.typePkgs[pkgPath] = tpkg
	l.astPkgs[pkgPath] = astFiles
	l.toGen[pkgPath] = make(map[string]bool)
	return nil
}

// Import satisfies the go/types.Importer interface.
//
// Unlike standard Go ones like go/importer and x/tools/go/packages, this one
// uses our own addPkg to instead load gunk packages.
//
// Aside from that, it is very similar to standard Go importers that load from
// source. It too uses a cache to avoid loading packages multiple times.
func (l *Loader) Import(path string) (*types.Package, error) {
	if tpkg := l.typePkgs[path]; tpkg != nil {
		return tpkg, nil
	}
	if err := l.addPkg(path); err != nil {
		return nil, err
	}
	if err := l.translatePkg(path); err != nil {
		return nil, err
	}
	return l.typePkgs[path], nil
}

// addProtoDep is called when a gunk file is known to require importing of a
// proto file, such as when using google.protobuf.Empty.
func (l *Loader) addProtoDep(protoPath string) {
	for _, dep := range l.pfile.Dependency {
		if dep == protoPath {
			return // already in there
		}
	}
	l.pfile.Dependency = append(l.pfile.Dependency, protoPath)
}

// loadProtoDeps loads all the proto dependencies added with addProtoDep.
//
// It does so with protoc, to leverage protoc's features such as locating the
// files, and the protoc parser to get a FileDescriptorProto out of the proto file
// content.
func (l *Loader) loadProtoDeps() error {
	missing := make(map[string]bool)
	for _, pfile := range l.allProto {
		for _, dep := range pfile.Dependency {
			if _, e := l.allProto[dep]; !e {
				missing[dep] = true
			}
		}
	}

	tmpl := template.Must(template.New("letter").Parse(`
syntax = "proto3";

{{range $dep, $_ := .}}import "{{$dep}}";
{{end}}
`))
	importsFile, err := os.Create("gunk-proto")
	if err != nil {
		return err
	}
	if err := tmpl.Execute(importsFile, missing); err != nil {
		return err
	}
	if err := importsFile.Close(); err != nil {
		return err
	}
	defer os.Remove("gunk-proto")

	// TODO: any way to specify stdout while being portable?
	cmd := exec.Command("protoc", "-o/dev/stdout", "--include_imports", "gunk-proto")
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("%s", e.Stderr)
		}
		return err
	}

	var fset desc.FileDescriptorSet
	if err := proto.Unmarshal(out, &fset); err != nil {
		return err
	}
	for _, pfile := range fset.File {
		if *pfile.Name == "gunk-proto" {
			continue
		}
		l.allProto[*pfile.Name] = pfile
	}
	return nil
}