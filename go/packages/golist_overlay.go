package packages

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

// processGolistOverlay provides rudimentary support for adding
// files that don't exist on disk to an overlay. The results can be
// sometimes incorrect.
// TODO(matloob): Handle unsupported cases, including the following:
// - determining the correct package to add given a new import path
func (state *golistState) processGolistOverlay(response *responseDeduper) (modifiedPkgs, needPkgs []string, err error) {
	havePkgs := make(map[string]string) // importPath -> non-test package ID
	needPkgsSet := make(map[string]bool)
	modifiedPkgsSet := make(map[string]bool)

	for _, pkg := range response.dr.Packages {
		// This is an approximation of import path to id. This can be
		// wrong for tests, vendored packages, and a number of other cases.
		havePkgs[pkg.PkgPath] = pkg.ID
	}

	// If no new imports are added, it is safe to avoid loading any needPkgs.
	// Otherwise, it's hard to tell which package is actually being loaded
	// (due to vendoring) and whether any modified package will show up
	// in the transitive set of dependencies (because new imports are added,
	// potentially modifying the transitive set of dependencies).
	var overlayAddsImports bool

	for opath, contents := range state.cfg.Overlay {
		base := filepath.Base(opath)
		dir := filepath.Dir(opath)
		var pkg *Package           // if opath belongs to both a package and its test variant, this will be the test variant
		var testVariantOf *Package // if opath is a test file, this is the package it is testing
		var fileExists bool
		isTestFile := strings.HasSuffix(opath, "_test.go")
		pkgName, ok := extractPackageName(opath, contents)
		if !ok {
			// Don't bother adding a file that doesn't even have a parsable package statement
			// to the overlay.
			continue
		}
	nextPackage:
		for _, p := range response.dr.Packages {
			if pkgName != p.Name && p.ID != "command-line-arguments" {
				continue
			}
			for _, f := range p.GoFiles {
				if !sameFile(filepath.Dir(f), dir) {
					continue
				}
				// Make sure to capture information on the package's test variant, if needed.
				if isTestFile && !hasTestFiles(p) {
					// TODO(matloob): Are there packages other than the 'production' variant
					// of a package that this can match? This shouldn't match the test main package
					// because the file is generated in another directory.
					testVariantOf = p
					continue nextPackage
				}
				if pkg != nil && p != pkg && pkg.PkgPath == p.PkgPath {
					// If we've already seen the test variant,
					// make sure to label which package it is a test variant of.
					if hasTestFiles(pkg) {
						testVariantOf = p
						continue nextPackage
					}
					// If we have already seen the package of which this is a test variant.
					if hasTestFiles(p) {
						testVariantOf = pkg
					}
				}
				pkg = p
				if filepath.Base(f) == base {
					fileExists = true
				}
			}
		}
		// The overlay could have included an entirely new package.
		if pkg == nil {
			// Try to find the module or gopath dir the file is contained in.
			// Then for modules, add the module opath to the beginning.
			pkgPath, ok, err := state.getPkgPath(dir)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				break
			}
			isXTest := strings.HasSuffix(pkgName, "_test")
			if isXTest {
				pkgPath += "_test"
			}
			id := pkgPath
			if isTestFile && !isXTest {
				id = fmt.Sprintf("%s [%s.test]", pkgPath, pkgPath)
			}
			// Try to reclaim a package with the same id if it exists in the response.
			for _, p := range response.dr.Packages {
				if reclaimPackage(p, id, opath, contents) {
					pkg = p
					break
				}
			}
			// Otherwise, create a new package
			if pkg == nil {
				pkg = &Package{PkgPath: pkgPath, ID: id, Name: pkgName, Imports: make(map[string]*Package)}
				response.addPackage(pkg)
				havePkgs[pkg.PkgPath] = id
				// Add the production package's sources for a test variant.
				if isTestFile && !isXTest && testVariantOf != nil {
					pkg.GoFiles = append(pkg.GoFiles, testVariantOf.GoFiles...)
					pkg.CompiledGoFiles = append(pkg.CompiledGoFiles, testVariantOf.CompiledGoFiles...)
				}
			}
		}
		if !fileExists {
			pkg.GoFiles = append(pkg.GoFiles, opath)
			// TODO(matloob): Adding the file to CompiledGoFiles can exhibit the wrong behavior
			// if the file will be ignored due to its build tags.
			pkg.CompiledGoFiles = append(pkg.CompiledGoFiles, opath)
			modifiedPkgsSet[pkg.ID] = true
		}
		imports, err := extractImports(opath, contents)
		if err != nil {
			// Let the parser or type checker report errors later.
			continue
		}
		for _, imp := range imports {
			if _, found := pkg.Imports[imp]; found {
				continue
			}

			// TODO(matloob): Handle cases when the following block isn't correct.
			// These include imports of vendored packages, etc.
			overlayAddsImports = true
			id, ok := havePkgs[imp]
			if !ok {
				id = imp
			}
			pkg.Imports[imp] = &Package{ID: id}
			// Add dependencies to the non-test variant version of this package as well.
			if testVariantOf != nil {
				testVariantOf.Imports[imp] = &Package{ID: id}
			}
		}
	}

	// toPkgPath tries to guess the package path given the id.
	// This isn't always correct -- it's certainly wrong for
	// vendored packages' paths.
	toPkgPath := func(id string) string {
		// TODO(matloob): Handle vendor paths.
		i := strings.IndexByte(id, ' ')
		if i >= 0 {
			return id[:i]
		}
		return id
	}

	// Now that new packages have been created, do another pass to determine
	// the new set of missing packages.
	for _, pkg := range response.dr.Packages {
		for _, imp := range pkg.Imports {
			pkgPath := toPkgPath(imp.ID)
			if _, ok := havePkgs[pkgPath]; !ok {
				needPkgsSet[pkgPath] = true
			}
		}
	}

	if overlayAddsImports {
		needPkgs = make([]string, 0, len(needPkgsSet))
		for pkg := range needPkgsSet {
			needPkgs = append(needPkgs, pkg)
		}
	}
	modifiedPkgs = make([]string, 0, len(modifiedPkgsSet))
	for pkg := range modifiedPkgsSet {
		modifiedPkgs = append(modifiedPkgs, pkg)
	}
	return modifiedPkgs, needPkgs, err
}

func hasTestFiles(p *Package) bool {
	for _, f := range p.GoFiles {
		if strings.HasSuffix(f, "_test.go") {
			return true
		}
	}
	return false
}

// determineRootDirs returns a mapping from absolute directories that could
// contain code to their corresponding import path prefixes.
func (state *golistState) determineRootDirs() (map[string]string, error) {
	env, err := state.getEnv()
	if err != nil {
		return nil, err
	}
	if env["GOMOD"] != "" {
		state.rootsOnce.Do(func() {
			state.rootDirs, state.rootDirsError = state.determineRootDirsModules()
		})
	} else {
		state.rootsOnce.Do(func() {
			state.rootDirs, state.rootDirsError = state.determineRootDirsGOPATH()
		})
	}
	return state.rootDirs, state.rootDirsError
}

func (state *golistState) determineRootDirsModules() (map[string]string, error) {
	out, err := state.invokeGo("list", "-m", "-json", "all")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	type jsonMod struct{ Path, Dir string }
	for dec := json.NewDecoder(out); dec.More(); {
		mod := new(jsonMod)
		if err := dec.Decode(mod); err != nil {
			return nil, err
		}
		if mod.Dir != "" && mod.Path != "" {
			// This is a valid module; add it to the map.
			absDir, err := filepath.Abs(mod.Dir)
			if err != nil {
				return nil, err
			}
			m[absDir] = mod.Path
		}
	}
	return m, nil
}

func (state *golistState) determineRootDirsGOPATH() (map[string]string, error) {
	m := map[string]string{}
	for _, dir := range filepath.SplitList(state.mustGetEnv()["GOPATH"]) {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		m[filepath.Join(absDir, "src")] = ""
	}
	return m, nil
}

func extractImports(filename string, contents []byte) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filename, contents, parser.ImportsOnly) // TODO(matloob): reuse fileset?
	if err != nil {
		return nil, err
	}
	var res []string
	for _, imp := range f.Imports {
		quotedPath := imp.Path.Value
		path, err := strconv.Unquote(quotedPath)
		if err != nil {
			return nil, err
		}
		res = append(res, path)
	}
	return res, nil
}

// reclaimPackage attempts to reuse a package that failed to load in an overlay.
//
// If the package has errors and has no Name, GoFiles, or Imports,
// then it's possible that it doesn't yet exist on disk.
func reclaimPackage(pkg *Package, id string, filename string, contents []byte) bool {
	// TODO(rstambler): Check the message of the actual error?
	// It differs between $GOPATH and module mode.
	if pkg.ID != id {
		return false
	}
	if len(pkg.Errors) != 1 {
		return false
	}
	if pkg.Name != "" || pkg.ExportFile != "" {
		return false
	}
	if len(pkg.GoFiles) > 0 || len(pkg.CompiledGoFiles) > 0 || len(pkg.OtherFiles) > 0 {
		return false
	}
	if len(pkg.Imports) > 0 {
		return false
	}
	pkgName, ok := extractPackageName(filename, contents)
	if !ok {
		return false
	}
	pkg.Name = pkgName
	pkg.Errors = nil
	return true
}

func extractPackageName(filename string, contents []byte) (string, bool) {
	// TODO(rstambler): Check the message of the actual error?
	// It differs between $GOPATH and module mode.
	f, err := parser.ParseFile(token.NewFileSet(), filename, contents, parser.PackageClauseOnly) // TODO(matloob): reuse fileset?
	if err != nil {
		return "", false
	}
	return f.Name.Name, true
}
