// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/gopls/internal/lsp/protocol"
	"golang.org/x/tools/gopls/internal/lsp/safetoken"
	"golang.org/x/tools/gopls/internal/span"
	"golang.org/x/tools/internal/diff"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/refactor/satisfy"
)

type renamer struct {
	ctx                context.Context
	refs               []*ReferenceInfo
	objsToUpdate       map[types.Object]bool
	hadConflicts       bool
	errors             string
	from, to           string
	satisfyConstraints map[satisfy.Constraint]bool
	packages           map[*types.Package]Package // may include additional packages that are a dep of pkg.
	msets              typeutil.MethodSetCache
	changeMethods      bool
}

type PrepareItem struct {
	Range protocol.Range
	Text  string
}

// PrepareRename searches for a valid renaming at position pp.
//
// The returned usererr is intended to be displayed to the user to explain why
// the prepare fails. Probably we could eliminate the redundancy in returning
// two errors, but for now this is done defensively.
func PrepareRename(ctx context.Context, snapshot Snapshot, f FileHandle, pp protocol.Position) (_ *PrepareItem, usererr, err error) {
	// Find position of the package name declaration.
	ctx, done := event.Start(ctx, "source.PrepareRename")
	defer done()

	// Is the cursor within the package name declaration?
	pgf, inPackageName, err := parsePackageNameDecl(ctx, snapshot, f, pp)
	if err != nil {
		return nil, err, err
	}
	if inPackageName {
		fileRenameSupported := false
		for _, op := range snapshot.View().Options().SupportedResourceOperations {
			if op == protocol.Rename {
				fileRenameSupported = true
				break
			}
		}

		if !fileRenameSupported {
			err := errors.New("can't rename package: LSP client does not support file renaming")
			return nil, err, err
		}
		fileMeta, err := snapshot.MetadataForFile(ctx, f.URI())
		if err != nil {
			return nil, err, err
		}

		if len(fileMeta) == 0 {
			err := fmt.Errorf("no packages found for file %q", f.URI())
			return nil, err, err
		}

		meta := fileMeta[0]

		if meta.Name == "main" {
			err := errors.New("can't rename package \"main\"")
			return nil, err, err
		}

		if strings.HasSuffix(string(meta.Name), "_test") {
			err := errors.New("can't rename x_test packages")
			return nil, err, err
		}

		if meta.Module == nil {
			err := fmt.Errorf("can't rename package: missing module information for package %q", meta.PkgPath)
			return nil, err, err
		}

		if meta.Module.Path == string(meta.PkgPath) {
			err := fmt.Errorf("can't rename package: package path %q is the same as module path %q", meta.PkgPath, meta.Module.Path)
			return nil, err, err
		}

		// Return the location of the package declaration.
		rng, err := pgf.Mapper.PosRange(pgf.File.Name.Pos(), pgf.File.Name.End())
		if err != nil {
			return nil, err, err
		}
		return &PrepareItem{
			Range: rng,
			Text:  string(meta.Name),
		}, nil, nil
	}

	qos, err := qualifiedObjsAtProtocolPos(ctx, snapshot, f.URI(), pp)
	if err != nil {
		return nil, nil, err
	}
	node, obj, pkg := qos[0].node, qos[0].obj, qos[0].sourcePkg
	if err := checkRenamable(obj); err != nil {
		return nil, nil, err
	}
	result, err := computePrepareRenameResp(snapshot, pkg, node, obj.Name())
	if err != nil {
		return nil, nil, err
	}
	return result, nil, nil
}

func computePrepareRenameResp(snapshot Snapshot, pkg Package, node ast.Node, text string) (*PrepareItem, error) {
	mr, err := posToMappedRange(pkg, node.Pos(), node.End())
	if err != nil {
		return nil, err
	}
	rng, err := mr.Range()
	if err != nil {
		return nil, err
	}
	if _, isImport := node.(*ast.ImportSpec); isImport {
		// We're not really renaming the import path.
		rng.End = rng.Start
	}
	return &PrepareItem{
		Range: rng,
		Text:  text,
	}, nil
}

// checkRenamable verifies if an obj may be renamed.
func checkRenamable(obj types.Object) error {
	if v, ok := obj.(*types.Var); ok && v.Embedded() {
		return errors.New("can't rename embedded fields: rename the type directly or name the field")
	}
	if obj.Name() == "_" {
		return errors.New("can't rename \"_\"")
	}
	return nil
}

// Rename returns a map of TextEdits for each file modified when renaming a
// given identifier within a package and a boolean value of true for renaming
// package and false otherwise.
func Rename(ctx context.Context, s Snapshot, f FileHandle, pp protocol.Position, newName string) (map[span.URI][]protocol.TextEdit, bool, error) {
	ctx, done := event.Start(ctx, "source.Rename")
	defer done()

	// Is the cursor within the package name declaration?
	_, inPackageName, err := parsePackageNameDecl(ctx, s, f, pp)
	if err != nil {
		return nil, false, err
	}
	if inPackageName {
		if !isValidIdentifier(newName) {
			return nil, true, fmt.Errorf("%q is not a valid identifier", newName)
		}

		fileMeta, err := s.MetadataForFile(ctx, f.URI())
		if err != nil {
			return nil, true, err
		}

		if len(fileMeta) == 0 {
			return nil, true, fmt.Errorf("no packages found for file %q", f.URI())
		}

		// We need metadata for the relevant package and module paths. These should
		// be the same for all packages containing the file.
		//
		// TODO(rfindley): we mix package path and import path here haphazardly.
		// Fix this.
		meta := fileMeta[0]
		oldPath := meta.PkgPath
		var modulePath PackagePath
		if mi := meta.Module; mi == nil {
			return nil, true, fmt.Errorf("cannot rename package: missing module information for package %q", meta.PkgPath)
		} else {
			modulePath = PackagePath(mi.Path)
		}

		if strings.HasSuffix(newName, "_test") {
			return nil, true, fmt.Errorf("cannot rename to _test package")
		}

		metadata, err := s.AllMetadata(ctx)
		if err != nil {
			return nil, true, err
		}

		renamingEdits, err := renamePackage(ctx, s, modulePath, oldPath, PackageName(newName), metadata)
		if err != nil {
			return nil, true, err
		}

		oldBase := filepath.Dir(span.URI.Filename(f.URI()))
		newPkgDir := filepath.Join(filepath.Dir(oldBase), newName)

		// TODO: should this operate on all go.mod files, irrespective of whether they are included in the workspace?
		// Get all active mod files in the workspace
		modFiles := s.ModFiles()
		for _, m := range modFiles {
			fh, err := s.GetFile(ctx, m)
			if err != nil {
				return nil, true, err
			}
			pm, err := s.ParseMod(ctx, fh)
			if err != nil {
				return nil, true, err
			}

			modFileDir := filepath.Dir(pm.URI.Filename())
			affectedReplaces := []*modfile.Replace{}

			// Check if any replace directives need to be fixed
			for _, r := range pm.File.Replace {
				if !strings.HasPrefix(r.New.Path, "/") && !strings.HasPrefix(r.New.Path, "./") && !strings.HasPrefix(r.New.Path, "../") {
					continue
				}

				replacedPath := r.New.Path
				if strings.HasPrefix(r.New.Path, "./") || strings.HasPrefix(r.New.Path, "../") {
					replacedPath = filepath.Join(modFileDir, r.New.Path)
				}

				// TODO: Is there a risk of converting a '\' delimited replacement to a '/' delimited replacement?
				if !strings.HasPrefix(filepath.ToSlash(replacedPath)+"/", filepath.ToSlash(oldBase)+"/") {
					continue // not affected by the package renaming
				}

				affectedReplaces = append(affectedReplaces, r)
			}

			if len(affectedReplaces) == 0 {
				continue
			}
			copied, err := modfile.Parse("", pm.Mapper.Content, nil)
			if err != nil {
				return nil, true, err
			}

			for _, r := range affectedReplaces {
				replacedPath := r.New.Path
				if strings.HasPrefix(r.New.Path, "./") || strings.HasPrefix(r.New.Path, "../") {
					replacedPath = filepath.Join(modFileDir, r.New.Path)
				}

				suffix := strings.TrimPrefix(replacedPath, string(oldBase))

				newReplacedPath, err := filepath.Rel(modFileDir, newPkgDir+suffix)
				if err != nil {
					return nil, true, err
				}

				newReplacedPath = filepath.ToSlash(newReplacedPath)

				if !strings.HasPrefix(newReplacedPath, "/") && !strings.HasPrefix(newReplacedPath, "../") {
					newReplacedPath = "./" + newReplacedPath
				}

				if err := copied.AddReplace(r.Old.Path, "", newReplacedPath, ""); err != nil {
					return nil, true, err
				}
			}

			copied.Cleanup()
			newContent, err := copied.Format()
			if err != nil {
				return nil, true, err
			}

			// Calculate the edits to be made due to the change.
			diff := s.View().Options().ComputeEdits(string(pm.Mapper.Content), string(newContent))
			modFileEdits, err := ToProtocolEdits(pm.Mapper, diff)
			if err != nil {
				return nil, true, err
			}

			renamingEdits[pm.URI] = append(renamingEdits[pm.URI], modFileEdits...)
		}

		return renamingEdits, true, nil
	}

	qos, err := qualifiedObjsAtProtocolPos(ctx, s, f.URI(), pp)
	if err != nil {
		return nil, false, err
	}
	result, err := renameObj(ctx, s, newName, qos)
	if err != nil {
		return nil, false, err
	}

	return result, false, nil
}

// renamePackage computes all workspace edits required to rename the package
// described by the given metadata, to newName, by renaming its package
// directory.
//
// It updates package clauses and import paths for the renamed package as well
// as any other packages affected by the directory renaming among packages
// described by allMetadata.
func renamePackage(ctx context.Context, s Snapshot, modulePath, oldPath PackagePath, newName PackageName, allMetadata []*Metadata) (map[span.URI][]protocol.TextEdit, error) {
	if modulePath == oldPath {
		return nil, fmt.Errorf("cannot rename package: module path %q is the same as the package path, so renaming the package directory would have no effect", modulePath)
	}

	newPathPrefix := path.Join(path.Dir(string(oldPath)), string(newName))

	edits := make(map[span.URI][]protocol.TextEdit)
	seen := make(seenPackageRename) // track per-file import renaming we've already processed

	// Rename imports to the renamed package from other packages.
	for _, m := range allMetadata {
		// Special case: x_test packages for the renamed package will not have the
		// package path as as a dir prefix, but still need their package clauses
		// renamed.
		if m.PkgPath == oldPath+"_test" {
			newTestName := newName + "_test"

			if err := renamePackageClause(ctx, m, s, newTestName, seen, edits); err != nil {
				return nil, err
			}
			continue
		}

		// Subtle: check this condition before checking for valid module info
		// below, because we should not fail this operation if unrelated packages
		// lack module info.
		if !strings.HasPrefix(string(m.PkgPath)+"/", string(oldPath)+"/") {
			continue // not affected by the package renaming
		}

		if m.Module == nil {
			// This check will always fail under Bazel.
			return nil, fmt.Errorf("cannot rename package: missing module information for package %q", m.PkgPath)
		}

		if modulePath != PackagePath(m.Module.Path) {
			continue // don't edit imports if nested package and renaming package have different module paths
		}

		// Renaming a package consists of changing its import path and package name.
		suffix := strings.TrimPrefix(string(m.PkgPath), string(oldPath))
		newPath := newPathPrefix + suffix

		pkgName := m.Name
		if m.PkgPath == PackagePath(oldPath) {
			pkgName = newName

			if err := renamePackageClause(ctx, m, s, newName, seen, edits); err != nil {
				return nil, err
			}
		}

		imp := ImportPath(newPath) // TODO(adonovan): what if newPath has vendor/ prefix?
		if err := renameImports(ctx, s, m, imp, pkgName, seen, edits); err != nil {
			return nil, err
		}
	}

	return edits, nil
}

// seenPackageRename tracks import path renamings that have already been
// processed.
//
// Due to test variants, files may appear multiple times in the reverse
// transitive closure of a renamed package, or in the reverse transitive
// closure of different variants of a renamed package (both are possible).
// However, in all cases the resulting edits will be the same.
type seenPackageRename map[seenPackageKey]bool
type seenPackageKey struct {
	uri  span.URI
	path PackagePath
}

// add reports whether uri and importPath have been seen, and records them as
// seen if not.
func (s seenPackageRename) add(uri span.URI, path PackagePath) bool {
	key := seenPackageKey{uri, path}
	seen := s[key]
	if !seen {
		s[key] = true
	}
	return seen
}

// renamePackageClause computes edits renaming the package clause of files in
// the package described by the given metadata, to newName.
//
// As files may belong to multiple packages, the seen map tracks files whose
// package clause has already been updated, to prevent duplicate edits.
//
// Edits are written into the edits map.
func renamePackageClause(ctx context.Context, m *Metadata, snapshot Snapshot, newName PackageName, seen seenPackageRename, edits map[span.URI][]protocol.TextEdit) error {
	// Rename internal references to the package in the renaming package.
	for _, uri := range m.CompiledGoFiles {
		if seen.add(uri, m.PkgPath) {
			continue
		}
		fh, err := snapshot.GetFile(ctx, uri)
		if err != nil {
			return err
		}
		f, err := snapshot.ParseGo(ctx, fh, ParseHeader)
		if err != nil {
			return err
		}
		if f.File.Name == nil {
			continue // no package declaration
		}
		rng, err := f.Mapper.PosRange(f.File.Name.Pos(), f.File.Name.End())
		if err != nil {
			return err
		}
		edits[f.URI] = append(edits[f.URI], protocol.TextEdit{
			Range:   rng,
			NewText: string(newName),
		})
	}

	return nil
}

// renameImports computes the set of edits to imports resulting from renaming
// the package described by the given metadata, to a package with import path
// newPath and name newName.
//
// Edits are written into the edits map.
func renameImports(ctx context.Context, snapshot Snapshot, m *Metadata, newPath ImportPath, newName PackageName, seen seenPackageRename, edits map[span.URI][]protocol.TextEdit) error {
	rdeps, err := snapshot.ReverseDependencies(ctx, m.ID, false) // find direct importers
	if err != nil {
		return err
	}

	// Pass 1: rename import paths in import declarations.
	needsTypeCheck := make(map[PackageID][]span.URI)
	for _, rdep := range rdeps {
		if rdep.IsIntermediateTestVariant() {
			continue // for renaming, these variants are redundant
		}

		for _, uri := range rdep.CompiledGoFiles {
			if seen.add(uri, m.PkgPath) {
				continue
			}
			fh, err := snapshot.GetFile(ctx, uri)
			if err != nil {
				return err
			}
			f, err := snapshot.ParseGo(ctx, fh, ParseHeader)
			if err != nil {
				return err
			}
			if f.File.Name == nil {
				continue // no package declaration
			}
			for _, imp := range f.File.Imports {
				if rdep.DepsByImpPath[UnquoteImportPath(imp)] != m.ID {
					continue // not the import we're looking for
				}

				// If the import does not explicitly specify
				// a local name, then we need to invoke the
				// type checker to locate references to update.
				if imp.Name == nil {
					needsTypeCheck[rdep.ID] = append(needsTypeCheck[rdep.ID], uri)
				}

				// Create text edit for the import path (string literal).
				impPathMappedRange := NewMappedRange(f.Mapper, imp.Path.Pos(), imp.Path.End())
				rng, err := impPathMappedRange.Range()
				if err != nil {
					return err
				}
				edits[uri] = append(edits[uri], protocol.TextEdit{
					Range:   rng,
					NewText: strconv.Quote(string(newPath)),
				})
			}
		}
	}

	// If the imported package's name hasn't changed,
	// we don't need to rename references within each file.
	if newName == m.Name {
		return nil
	}

	// Pass 2: rename local name (types.PkgName) of imported
	// package throughout one or more files of the package.
	ids := make([]PackageID, 0, len(needsTypeCheck))
	for id := range needsTypeCheck {
		ids = append(ids, id)
	}
	pkgs, err := snapshot.TypeCheck(ctx, TypecheckFull, ids...)
	if err != nil {
		return err
	}
	for i, id := range ids {
		pkg := pkgs[i]
		for _, uri := range needsTypeCheck[id] {
			f, err := pkg.File(uri)
			if err != nil {
				return err
			}
			for _, imp := range f.File.Imports {
				if imp.Name != nil {
					continue // has explicit local name
				}
				if rdeps[id].DepsByImpPath[UnquoteImportPath(imp)] != m.ID {
					continue // not the import we're looking for
				}

				pkgname := pkg.GetTypesInfo().Implicits[imp].(*types.PkgName)
				qos := []qualifiedObject{{obj: pkgname, pkg: pkg}}

				pkgScope := pkg.GetTypes().Scope()
				fileScope := pkg.GetTypesInfo().Scopes[f.File]

				localName := string(newName)
				try := 0

				// Keep trying with fresh names until one succeeds.
				for fileScope.Lookup(localName) != nil || pkgScope.Lookup(localName) != nil {
					try++
					localName = fmt.Sprintf("%s%d", newName, try)
				}
				changes, err := renameObj(ctx, snapshot, localName, qos)
				if err != nil {
					return err
				}

				// If the chosen local package name matches the package's
				// new name, delete the change that would have inserted
				// an explicit local name, which is always the lexically
				// first change.
				if localName == string(newName) {
					v := changes[uri]
					sort.Slice(v, func(i, j int) bool {
						return protocol.CompareRange(v[i].Range, v[j].Range) < 0
					})
					changes[uri] = v[1:]
				}
				for uri, changeEdits := range changes {
					edits[uri] = append(edits[uri], changeEdits...)
				}
			}
		}
	}
	return nil
}

// renameObj returns a map of TextEdits for renaming an identifier within a file
// and boolean value of true if there is no renaming conflicts and false otherwise.
func renameObj(ctx context.Context, s Snapshot, newName string, qos []qualifiedObject) (map[span.URI][]protocol.TextEdit, error) {
	obj := qos[0].obj

	if err := checkRenamable(obj); err != nil {
		return nil, err
	}
	if obj.Name() == newName {
		return nil, fmt.Errorf("old and new names are the same: %s", newName)
	}
	if !isValidIdentifier(newName) {
		return nil, fmt.Errorf("invalid identifier to rename: %q", newName)
	}
	refs, err := references(ctx, s, qos, true, false, true)
	if err != nil {
		return nil, err
	}
	r := renamer{
		ctx:          ctx,
		refs:         refs,
		objsToUpdate: make(map[types.Object]bool),
		from:         obj.Name(),
		to:           newName,
		packages:     make(map[*types.Package]Package),
	}

	// A renaming initiated at an interface method indicates the
	// intention to rename abstract and concrete methods as needed
	// to preserve assignability.
	for _, ref := range refs {
		if obj, ok := ref.obj.(*types.Func); ok {
			recv := obj.Type().(*types.Signature).Recv()
			if recv != nil && IsInterface(recv.Type().Underlying()) {
				r.changeMethods = true
				break
			}
		}
	}
	for _, from := range refs {
		r.packages[from.pkg.GetTypes()] = from.pkg
	}

	// Check that the renaming of the identifier is ok.
	for _, ref := range refs {
		r.check(ref.obj)
		if r.hadConflicts { // one error is enough.
			break
		}
	}
	if r.hadConflicts {
		return nil, fmt.Errorf(r.errors)
	}

	changes, err := r.update()
	if err != nil {
		return nil, err
	}

	result := make(map[span.URI][]protocol.TextEdit)
	for uri, edits := range changes {
		// These edits should really be associated with FileHandles for maximal correctness.
		// For now, this is good enough.
		fh, err := s.GetFile(ctx, uri)
		if err != nil {
			return nil, err
		}
		data, err := fh.Read()
		if err != nil {
			return nil, err
		}
		m := protocol.NewColumnMapper(uri, data)
		protocolEdits, err := ToProtocolEdits(m, edits)
		if err != nil {
			return nil, err
		}
		result[uri] = protocolEdits
	}
	return result, nil
}

// Rename all references to the identifier.
func (r *renamer) update() (map[span.URI][]diff.Edit, error) {
	result := make(map[span.URI][]diff.Edit)
	seen := make(map[span.Span]bool)

	docRegexp, err := regexp.Compile(`\b` + r.from + `\b`)
	if err != nil {
		return nil, err
	}
	for _, ref := range r.refs {
		refSpan, err := ref.Span()
		if err != nil {
			return nil, err
		}
		if seen[refSpan] {
			continue
		}
		seen[refSpan] = true

		// Renaming a types.PkgName may result in the addition or removal of an identifier,
		// so we deal with this separately.
		if pkgName, ok := ref.obj.(*types.PkgName); ok && ref.isDeclaration {
			edit, err := r.updatePkgName(pkgName)
			if err != nil {
				return nil, err
			}
			result[refSpan.URI()] = append(result[refSpan.URI()], *edit)
			continue
		}

		// Replace the identifier with r.to.
		edit := diff.Edit{
			Start: refSpan.Start().Offset(),
			End:   refSpan.End().Offset(),
			New:   r.to,
		}

		result[refSpan.URI()] = append(result[refSpan.URI()], edit)

		if !ref.isDeclaration || ref.ident == nil { // uses do not have doc comments to update.
			continue
		}

		doc := r.docComment(ref.pkg, ref.ident)
		if doc == nil {
			continue
		}

		// Perform the rename in doc comments declared in the original package.
		// go/parser strips out \r\n returns from the comment text, so go
		// line-by-line through the comment text to get the correct positions.
		for _, comment := range doc.List {
			if isDirective(comment.Text) {
				continue
			}
			// TODO(adonovan): why are we looping over lines?
			// Just run the loop body once over the entire multiline comment.
			lines := strings.Split(comment.Text, "\n")
			tokFile := ref.pkg.FileSet().File(comment.Pos())
			commentLine := tokFile.Line(comment.Pos())
			uri := span.URIFromPath(tokFile.Name())
			for i, line := range lines {
				lineStart := comment.Pos()
				if i > 0 {
					lineStart = tokFile.LineStart(commentLine + i)
				}
				for _, locs := range docRegexp.FindAllIndex([]byte(line), -1) {
					// The File.Offset static check complains
					// even though these uses are manifestly safe.
					start, _ := safetoken.Offset(tokFile, lineStart+token.Pos(locs[0]))
					end, _ := safetoken.Offset(tokFile, lineStart+token.Pos(locs[1]))
					result[uri] = append(result[uri], diff.Edit{
						Start: start,
						End:   end,
						New:   r.to,
					})
				}
			}
		}
	}

	return result, nil
}

// docComment returns the doc for an identifier.
func (r *renamer) docComment(pkg Package, id *ast.Ident) *ast.CommentGroup {
	_, tokFile, nodes, _ := pathEnclosingInterval(pkg, id.Pos(), id.End())
	for _, node := range nodes {
		switch decl := node.(type) {
		case *ast.FuncDecl:
			return decl.Doc
		case *ast.Field:
			return decl.Doc
		case *ast.GenDecl:
			return decl.Doc
		// For {Type,Value}Spec, if the doc on the spec is absent,
		// search for the enclosing GenDecl
		case *ast.TypeSpec:
			if decl.Doc != nil {
				return decl.Doc
			}
		case *ast.ValueSpec:
			if decl.Doc != nil {
				return decl.Doc
			}
		case *ast.Ident:
		case *ast.AssignStmt:
			// *ast.AssignStmt doesn't have an associated comment group.
			// So, we try to find a comment just before the identifier.

			// Try to find a comment group only for short variable declarations (:=).
			if decl.Tok != token.DEFINE {
				return nil
			}

			identLine := tokFile.Line(id.Pos())
			for _, comment := range nodes[len(nodes)-1].(*ast.File).Comments {
				if comment.Pos() > id.Pos() {
					// Comment is after the identifier.
					continue
				}

				lastCommentLine := tokFile.Line(comment.End())
				if lastCommentLine+1 == identLine {
					return comment
				}
			}
		default:
			return nil
		}
	}
	return nil
}

// updatePkgName returns the updates to rename a pkgName in the import spec by
// only modifying the package name portion of the import declaration.
func (r *renamer) updatePkgName(pkgName *types.PkgName) (*diff.Edit, error) {
	// Modify ImportSpec syntax to add or remove the Name as needed.
	pkg := r.packages[pkgName.Pkg()]
	_, tokFile, path, _ := pathEnclosingInterval(pkg, pkgName.Pos(), pkgName.Pos())
	if len(path) < 2 {
		return nil, fmt.Errorf("no path enclosing interval for %s", pkgName.Name())
	}
	spec, ok := path[1].(*ast.ImportSpec)
	if !ok {
		return nil, fmt.Errorf("failed to update PkgName for %s", pkgName.Name())
	}

	newText := ""
	if pkgName.Imported().Name() != r.to {
		newText = r.to + " "
	}

	// Replace the portion (possibly empty) of the spec before the path:
	//     local "path"      or      "path"
	//   ->      <-                -><-
	rng := span.NewRange(tokFile, spec.Pos(), spec.Path.Pos())
	spn, err := rng.Span()
	if err != nil {
		return nil, err
	}

	return &diff.Edit{
		Start: spn.Start().Offset(),
		End:   spn.End().Offset(),
		New:   newText,
	}, nil
}
