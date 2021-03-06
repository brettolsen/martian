//
// Copyright (c) 2014 10X Genomics, Inc. All rights reserved.
//
// MRO semantic checking.
//

package syntax

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/martian-lang/martian/martian/util"
)

//
// Semantic Checking Methods
//
func (global *Ast) err(nodable AstNodable, msg string, v ...interface{}) error {
	return &AstError{global, nodable.getNode(), fmt.Sprintf(msg, v...)}
}

func (global *Ast) compile() error {
	if err := global.compileTypes(); err != nil {
		return err
	}

	// Check for duplicate names amongst callables.
	if err := global.Callables.compile(global); err != nil {
		return err
	}

	if err := global.compileStages(); err != nil {
		return err
	}

	if err := global.compilePipelineDecs(); err != nil {
		return err
	}

	if err := global.compilePipelineArgs(); err != nil {
		return err
	}

	if err := global.compileCall(); err != nil {
		return err
	}

	return nil
}

func (global *Ast) checkSrcPaths(stagecodePaths []string) error {
	var errs ErrorList
	for _, stage := range global.Stages {
		// Exempt exec stages
		if stage.Src.Lang != "exec" && stage.Src.Lang != "comp" {
			if _, found := util.SearchPaths(stage.Src.Path, stagecodePaths); !found {
				stagecodePathsList := strings.Join(stagecodePaths, ", ")
				errs = append(errs, global.err(stage,
					"SourcePathError: searched (%s) but stage source path not found '%s'",
					stagecodePathsList, stage.Src.Path))
			}
		}
	}
	return errs.If()
}

func (src *SourceFile) checkIncludes(fullPath string, inc *SourceLoc) error {
	var errs ErrorList
	if fullPath == src.FullPath {
		errs = append(errs, &wrapError{
			innerError: fmt.Errorf("Include cycle: %s included", src.FullPath),
			loc:        *inc,
		})
	} else {
		for _, parent := range src.IncludedFrom {
			if err := parent.File.checkIncludes(fullPath, inc); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs.If()
}

// A Parser object allows the ParseSourceBytes and Compile methods
// to cache state if repeatedly invoked.
//
// The Parser object is NOT thread safe.
type Parser struct {
	intern *stringIntern
}

// ParseSource parses a souce string into an ast.
//
// src is the mro source code.
//
// srcpath is the path to the source code file (if applicable), used for
// debugging information.
//
// incpaths is the orderd set of search paths to use when resolving include
// directives.
//
// if checksrc is true, then the parser will verify that stage src values
// refer to code that actually exists.
func ParseSource(src string, srcPath string,
	incPaths []string, checkSrc bool) (string, []string, *Ast, error) {
	return ParseSourceBytes([]byte(src), srcPath, incPaths, checkSrc)
}

// ParseSourceBytes parses a source byte array into an ast.
//
// src is the mro source code.
//
// srcpath is the path to the source code file (if applicable), used for
// debugging information.
//
// incpaths is the orderd set of search paths to use when resolving include
// directives.
//
// if checksrc is true, then the parser will verify that stage src values
// refer to code that actually exists.
func ParseSourceBytes(src []byte, srcPath string,
	incPaths []string, checkSrc bool) (string, []string, *Ast, error) {
	var parser Parser
	return parser.ParseSourceBytes(src, srcPath, incPaths, checkSrc)
}

func (parser *Parser) getIntern() *stringIntern {
	if parser == nil {
		return makeStringIntern()
	} else if parser.intern == nil {
		parser.intern = makeStringIntern()
	}
	return parser.intern
}

// ParseSourceBytes parses a source byte array into an ast.
//
// src is the mro source code.
//
// srcpath is the path to the source code file (if applicable), used for
// debugging information.
//
// incpaths is the orderd set of search paths to use when resolving include
// directives.
//
// if checksrc is true, then the parser will verify that stage src values
// refer to code that actually exists.
func (parser *Parser) ParseSourceBytes(src []byte, srcPath string,
	incPaths []string, checkSrc bool) (string, []string, *Ast, error) {
	fname := filepath.Base(srcPath)
	absPath, _ := filepath.Abs(srcPath)
	srcFile := SourceFile{
		FileName: fname,
		FullPath: absPath,
	}
	if ast, err := parseSource(src, &srcFile, incPaths,
		map[string]*SourceFile{absPath: &srcFile},
		parser.getIntern()); err != nil {
		return "", nil, ast, err
	} else {
		err := ast.compile()
		ifnames := make([]string, len(ast.Includes))
		for i, inc := range ast.Includes {
			ifnames[i] = inc.Value
		}
		if checkSrc {
			stagecodePaths := filepath.SplitList(os.Getenv("PATH"))
			seenPaths := make(map[string]struct{}, len(incPaths)+len(stagecodePaths))
			for f := range ast.Files {
				p := filepath.Dir(f)
				if _, ok := seenPaths[p]; !ok {
					stagecodePaths = append(stagecodePaths, p)
					seenPaths[p] = struct{}{}
				}
			}
			if srcerr := ast.checkSrcPaths(stagecodePaths); srcerr != nil {
				err = ErrorList{err, srcerr}.If()
			}
		}
		return ast.format(false), ifnames, ast, err
	}
}

func parseSource(src []byte, srcFile *SourceFile, incPaths []string,
	processedIncludes map[string]*SourceFile, intern *stringIntern) (*Ast, error) {
	// Add the source file's own folder to the include path for
	// resolving both @includes and stage src paths.
	incPaths = append([]string{filepath.Dir(srcFile.FullPath)}, incPaths...)

	// Parse the source into an AST and attach the comments.
	ast, err := yaccParse(src, srcFile, intern)
	if err != nil {
		return nil, err
	}

	iasts, err := getIncludes(srcFile, ast.Includes, incPaths, processedIncludes, intern)
	if iasts != nil {
		ast.merge(iasts)
	}
	return ast, err
}

func getIncludes(srcFile *SourceFile, includes []*Include, incPaths []string,
	processedIncludes map[string]*SourceFile, intern *stringIntern) (*Ast, error) {
	var errs ErrorList
	var iasts *Ast
	seen := make(map[string]struct{}, len(includes))
	for _, inc := range includes {
		if ifpath, found := util.SearchPaths(inc.Value, incPaths); !found {
			errs = append(errs, &FileNotFoundError{
				name: inc.Value,
				loc:  inc.Node.Loc,
			})
		} else {
			absPath, _ := filepath.Abs(ifpath)
			if _, ok := seen[absPath]; ok {
				errs = append(errs, &wrapError{
					innerError: fmt.Errorf("%s included multiple times",
						inc.Value),
					loc: inc.Node.Loc,
				})
			}
			seen[absPath] = struct{}{}

			if absPath == srcFile.FullPath {
				errs = append(errs, &wrapError{
					innerError: fmt.Errorf("%s includes itself", srcFile.FullPath),
					loc:        inc.Node.Loc,
				})
			} else if iSrcFile := processedIncludes[absPath]; iSrcFile != nil {
				iSrcFile.IncludedFrom = append(iSrcFile.IncludedFrom, &inc.Node.Loc)
				if err := srcFile.checkIncludes(absPath, &inc.Node.Loc); err != nil {
					errs = append(errs, err)
				}
			} else {
				iSrcFile = &SourceFile{
					FileName:     inc.Value,
					FullPath:     absPath,
					IncludedFrom: []*SourceLoc{&inc.Node.Loc},
				}
				processedIncludes[absPath] = iSrcFile
				if b, err := ioutil.ReadFile(iSrcFile.FullPath); err != nil {
					errs = append(errs, &wrapError{
						innerError: err,
						loc:        inc.Node.Loc,
					})
				} else {
					iast, err := parseSource(b, iSrcFile,
						incPaths[1:], processedIncludes, intern)
					errs = append(errs, err)
					if iast != nil {
						if iasts == nil {
							iasts = iast
						} else {
							// x.merge(y) puts y's stuff before x's.
							iast.merge(iasts)
							iasts = iast
						}
					}
				}
			}
		}
	}
	return iasts, errs.If()
}

// Compile an MRO file in cwd or mroPaths.
//
// fpath is the path (absolute or relative to the current working directory) of
// the source file.
//
// mroPaths specifies additional paths in which to search files requested with
// @include
//
// If checkcSrcPath is true, an error will be returned if the src parameter in
// a stage definition does not refer to an existing path.
//
// Returns the combined source (after processing all includes), the transitive
// closure of all includes, the compiled AST, or an error if applicable.
func Compile(fpath string,
	mroPaths []string, checkSrcPath bool) (string, []string, *Ast, error) {
	var parser Parser
	return parser.Compile(fpath, mroPaths, checkSrcPath)
}

// Compile an MRO file in cwd or mroPaths.
//
// fpath is the path (absolute or relative to the current working directory) of
// the source file.
//
// mroPaths specifies additional paths in which to search files requested with
// @include
//
// If checkcSrcPath is true, an error will be returned if the src parameter in
// a stage definition does not refer to an existing path.
//
// Returns the combined source (after processing all includes), the transitive
// closure of all includes, the compiled AST, or an error if applicable.
func (parser *Parser) Compile(fpath string,
	mroPaths []string, checkSrcPath bool) (string, []string, *Ast, error) {

	if data, err := ioutil.ReadFile(fpath); err != nil {
		return "", nil, nil, err
	} else {
		return parser.ParseSourceBytes(data, fpath, mroPaths, checkSrcPath)
	}
}
