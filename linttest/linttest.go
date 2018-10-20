package linttest

import (
	"go/ast"
	"go/parser"
	"go/types"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/go-lintpack/lintpack"
	"golang.org/x/tools/go/loader"
)

var sizes = types.SizesFor("gc", runtime.GOARCH)

func saneCheckersList(t *testing.T) []*lintpack.CheckerInfo {
	var saneList []*lintpack.CheckerInfo

	for _, info := range lintpack.GetCheckersInfo() {
		pkgPath := "github.com/go-lintpack/lintpack/linttest/testdata/sanity"
		t.Run("sanity/"+info.Name, func(t *testing.T) {
			prog := newProg(t, pkgPath)
			pkgInfo := prog.Imported[pkgPath]
			ctx := &lintpack.Context{
				SizesInfo: sizes,
				FileSet:   prog.Fset,
				TypesInfo: &pkgInfo.Info,
				Pkg:       pkgInfo.Pkg,
			}
			c := lintpack.NewChecker(ctx, info)
			defer func() {
				r := recover()
				if r != nil {
					t.Errorf("unexpected panic: %v\n%s", r, debug.Stack())
				} else {
					saneList = append(saneList, info)
				}
			}()
			for _, f := range pkgInfo.Files {
				ctx.SetFileInfo(getFilename(prog, f), f)
				_ = c.Check(f)
			}
		})
	}

	return saneList
}

// TestCheckers runs end2end tests over all registered checkers using default options.
//
// TODO(Quasilyte): document default options.
// TODO(Quasilyte): make it possible to run tests with different options.
func TestCheckers(t *testing.T) {
	for _, info := range saneCheckersList(t) {
		t.Run(info.Name, func(t *testing.T) {
			pkgPath := "./testdata/" + info.Name

			prog := newProg(t, pkgPath)
			pkgInfo := prog.Imported[pkgPath]
			ctx := &lintpack.Context{
				SizesInfo: sizes,
				FileSet:   prog.Fset,
				TypesInfo: &pkgInfo.Info,
				Pkg:       pkgInfo.Pkg,
			}
			c := lintpack.NewChecker(ctx, info)

			checkFiles(t, c, ctx, prog, pkgPath)
		})
	}
}

func checkFiles(t *testing.T, c *lintpack.Checker, ctx *lintpack.Context, prog *loader.Program, pkgPath string) {
	files := prog.Imported[pkgPath].Files

	for _, f := range files {
		filename := getFilename(prog, f)
		testFilename := filepath.Join("testdata", c.Info.Name, filename)
		goldenWarns := newGoldenFile(t, testFilename)

		stripDirectives(f)
		ctx.SetFileInfo(filename, f)

		for _, warn := range c.Check(f) {
			line := ctx.FileSet.Position(warn.Node.Pos()).Line

			if w := goldenWarns.find(line, warn.Text); w != nil {
				if w.matched {
					t.Errorf("%s:%d: multiple matches for %s",
						testFilename, line, w)
				}
				w.matched = true
			} else {
				t.Errorf("%s:%d: unexpected warn: %s",
					testFilename, line, warn.Text)
			}
		}

		goldenWarns.checkUnmatched(t, testFilename)
	}
}

// stripDirectives replaces "///" comments with empty single-line
// comments, so the checkers that inspect comments see ordinary
// comment groups (with extra newlines, but that's not important).
func stripDirectives(f *ast.File) {
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "/// ") {
				c.Text = "//"
			}
		}
	}
}

func getFilename(prog *loader.Program, f *ast.File) string {
	// see https://github.com/golang/go/issues/24498
	return filepath.Base(prog.Fset.Position(f.Pos()).Filename)
}

func newProg(t *testing.T, pkgPath string) *loader.Program {
	conf := loader.Config{
		ParserMode: parser.ParseComments,
		TypeChecker: types.Config{
			Sizes: sizes,
		},
	}
	if _, err := conf.FromArgs([]string{pkgPath}, true); err != nil {
		t.Fatalf("resolve packages: %v", err)
	}
	prog, err := conf.Load()
	if err != nil {
		t.Fatal(err)
	}
	pkgInfo := prog.Imported[pkgPath]
	if pkgInfo == nil || !pkgInfo.TransitivelyErrorFree {
		t.Fatalf("%s package is not properly loaded", pkgPath)
	}
	return prog
}
