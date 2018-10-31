package check

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/go-lintpack/lintpack"
	"github.com/go-lintpack/lintpack/linter/lintmain/internal/hotload"
	"github.com/logrusorgru/aurora"
	"golang.org/x/tools/go/loader"
)

// Main implements sub-command entry point.
func Main() {
	var l linter

	steps := []struct {
		name string
		fn   func() error
	}{
		{"parse args", l.parseArgs},
		{"load program", l.loadProgram},
		{"load plugin", l.loadPlugin},
		{"init checkers", l.initCheckers},
		{"run checkers", l.runCheckers},
		{"exit if found issues", l.exit},
	}

	for _, step := range steps {
		if err := step.fn(); err != nil {
			log.Fatalf("%s: %v", step.name, err)
		}
	}
}

type linter struct {
	ctx *lintpack.Context

	prog *loader.Program

	checkers []*lintpack.Checker

	packages []string

	foundIssues bool

	filters struct {
		disableTags *regexp.Regexp
		disable     *regexp.Regexp
		enableTags  *regexp.Regexp
		enable      *regexp.Regexp
	}

	pluginPath string

	exitCode           int
	checkTests         bool
	checkGenerated     bool
	shorterErrLocation bool
	coloredOutput      bool
	verbose            bool
}

func (l *linter) exit() error {
	if l.foundIssues {
		os.Exit(l.exitCode)
	}
	return nil
}

func (l *linter) runCheckers() error {
	pkgInfoMap := make(map[string]*loader.PackageInfo)
	for _, pkgInfo := range l.prog.AllPackages {
		pkgInfoMap[pkgInfo.Pkg.Path()] = pkgInfo
	}
	for _, pkgPath := range l.packages {
		pkgInfo := pkgInfoMap[pkgPath]
		if pkgInfo == nil || !pkgInfo.TransitivelyErrorFree {
			log.Fatalf("%s package is not properly loaded", pkgPath)
		}
		// Check the package itself.
		l.checkPackage(pkgPath, pkgInfo)
		// Check package external test (if any).
		pkgInfo = pkgInfoMap[pkgPath+"_test"]
		if pkgInfo != nil {
			l.checkPackage(pkgPath+"_test", pkgInfo)
		}
	}

	return nil
}

func (l *linter) checkPackage(pkgPath string, pkgInfo *loader.PackageInfo) {
	l.ctx.SetPackageInfo(&pkgInfo.Info, pkgInfo.Pkg)
	for _, f := range pkgInfo.Files {
		filename := l.getFilename(f)
		if !l.checkTests && strings.HasSuffix(filename, "_test.go") {
			continue
		}
		if !l.checkGenerated && l.isGenerated(f) {
			continue
		}
		l.ctx.SetFileInfo(filename, f)
		l.checkFile(f)
	}
}

func (l *linter) checkFile(f *ast.File) {
	var wg sync.WaitGroup
	wg.Add(len(l.checkers))
	for _, c := range l.checkers {
		// All checkers are expected to use *lint.Context
		// as read-only structure, so no copying is required.
		go func(c *lintpack.Checker) {
			defer func() {
				wg.Done()
				// Checker signals unexpected error with panic(error).
				r := recover()
				if r == nil {
					return // There were no panic
				}
				if err, ok := r.(error); ok {
					log.Printf("%s: error: %v\n", c.Info.Name, err)
					panic(err)
				} else {
					// Some other kind of run-time panic.
					// Undo the recover and resume panic.
					panic(r)
				}
			}()

			for _, warn := range c.Check(f) {
				l.foundIssues = true
				loc := l.ctx.FileSet.Position(warn.Node.Pos()).String()
				if l.shorterErrLocation {
					loc = shortenLocation(loc)
				}

				printWarning(l, c.Info.Name, loc, warn.Text)
			}
		}(c)
	}
	wg.Wait()
}

func (l *linter) initCheckers() error {
	matchAnyTag := func(re *regexp.Regexp, info *lintpack.CheckerInfo) bool {
		for _, tag := range info.Tags {
			if re.MatchString(tag) {
				return true
			}
		}
		return false
	}
	disabledByTags := func(info *lintpack.CheckerInfo) bool {
		if len(info.Tags) == 0 {
			return false
		}
		return matchAnyTag(l.filters.disableTags, info)
	}
	enabledByTags := func(info *lintpack.CheckerInfo) bool {
		if len(info.Tags) == 0 {
			return true
		}
		return matchAnyTag(l.filters.enableTags, info)
	}

	for _, info := range lintpack.GetCheckersInfo() {
		enabled := false
		notice := ""

		switch {
		case disabledByTags(info):
			notice = "disabled by tags (-disableTags)"
		case l.filters.disable.MatchString(info.Name):
			notice = "disabled by name (-disable)"
		case enabledByTags(info):
			enabled = true
			notice = "enabled by tags (-enableTags)"
		case l.filters.enable.MatchString(info.Name):
			enabled = true
			notice = "enabled by name (-enable)"
		default:
			notice = "was not enabled"
		}

		if l.verbose {
			log.Printf("\tdebug: %s: %s", info.Name, notice)
		}
		if enabled {
			l.checkers = append(l.checkers, lintpack.NewChecker(l.ctx, info))
		}
	}

	if len(l.checkers) == 0 {
		return errors.New("empty checkers set selected")
	}
	return nil
}

func (l *linter) loadProgram() error {
	sizes := types.SizesFor("gc", runtime.GOARCH)
	if sizes == nil {
		return fmt.Errorf("can't find sizes info for %s", runtime.GOARCH)
	}

	conf := loader.Config{
		ParserMode: parser.ParseComments,
		TypeChecker: types.Config{
			Sizes: sizes,
		},
	}

	if _, err := conf.FromArgs(l.packages, true); err != nil {
		log.Fatalf("resolve packages: %v", err)
	}
	prog, err := conf.Load()
	if err != nil {
		log.Fatalf("load program: %v", err)
	}

	l.prog = prog
	l.ctx = lintpack.NewContext(prog.Fset, sizes)

	return nil
}

func (l *linter) loadPlugin() error {
	return hotload.CheckersFromDylib(l.pluginPath)
}

func (l *linter) parseArgs() error {
	flag.StringVar(&l.pluginPath, "pluginPath", "",
		`path to a Go plugin that provides additional checks`)
	disableTags := flag.String("disableTags", `^experimental$|^performance$|^opinionated$`,
		`regexp that excludes checkers that have matching tag`)
	disable := flag.String("disable", `<none>`,
		`regexp that disables unwanted checks`)
	enableTags := flag.String("enableTags", `.*`,
		`regexp that includes checkers that have matching tag`)
	enable := flag.String("enable", `.*`,
		`regexp that selects what checkers are being run. Applied after all other filters`)
	flag.IntVar(&l.exitCode, "exitCode", 1,
		`exit code to be used when lint issues are found`)
	flag.BoolVar(&l.checkTests, "checkTests", true,
		`whether to check test files`)
	flag.BoolVar(&l.shorterErrLocation, `shorterErrLocation`, true,
		`whether to replace error location prefix with $GOROOT and $GOPATH`)
	flag.BoolVar(&l.coloredOutput, `coloredOutput`, true,
		`whether to use colored output`)
	flag.BoolVar(&l.verbose, `verbose`, false,
		`whether to print output useful during linter debugging`)

	flag.Parse()

	var err error

	l.packages = flag.Args()
	l.filters.disableTags, err = regexp.Compile(*disableTags)
	if err != nil {
		return fmt.Errorf("-disableTags: %v", err)
	}
	l.filters.disable, err = regexp.Compile(*disable)
	if err != nil {
		return fmt.Errorf("-disable: %v", err)
	}
	l.filters.enableTags, err = regexp.Compile(*enableTags)
	if err != nil {
		return fmt.Errorf("-enableTags: %v", err)
	}
	l.filters.enable, err = regexp.Compile(*enable)
	if err != nil {
		return fmt.Errorf("-enable: %v", err)
	}

	return nil
}

var generatedFileCommentRE = regexp.MustCompile("Code generated .* DO NOT EDIT.")

func (l *linter) isGenerated(f *ast.File) bool {
	return len(f.Comments) != 0 &&
		generatedFileCommentRE.MatchString(f.Comments[0].Text())
}

func (l *linter) getFilename(f *ast.File) string {
	// See https://github.com/golang/go/issues/24498.
	return filepath.Base(l.prog.Fset.Position(f.Pos()).Filename)
}

func shortenLocation(loc string) string {
	switch {
	case strings.HasPrefix(loc, build.Default.GOPATH):
		return strings.Replace(loc, build.Default.GOPATH, "$GOPATH", 1)
	case strings.HasPrefix(loc, build.Default.GOROOT):
		return strings.Replace(loc, build.Default.GOROOT, "$GOROOT", 1)
	default:
		return loc
	}
}

func printWarning(l *linter, rule, loc, warn string) {
	switch {
	case l.coloredOutput:
		log.Printf("%v: %v: %v\n",
			aurora.Magenta(aurora.Bold(loc)),
			aurora.Red(rule),
			warn)

	default:
		log.Printf("%s: %s: %s\n", loc, rule, warn)
	}
}
