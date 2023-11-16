package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	fset := token.NewFileSet()
	dir := "test/e2e"
	t := time.Now().Format("2006-01-02-15-04-05")
	dest := filepath.Join("/tmp", t)
	err := os.MkdirAll(dest, 0755)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("created dir: ", dest)
	walkDir := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, "_test.go") {
			parsedFile, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				log.Fatal(err)
			}
			visitor := &FuncVisitor{}
			ast.Walk(visitor, parsedFile)
			if len(visitor.funcs) == 0 {
				fmt.Println("no test function found for file: ", path)
				return nil
			}
			body := strings.Join(visitor.funcs, "|")
			r := fmt.Sprintf("^(%s)$", body)
			cmd := "go test -v -timeout=75m  antrea.io/antrea/test/e2e -provider=kind --coverage --coverage-dir=%s -run \"%s\""
			covDir := filepath.Join(dest, strings.Split(filepath.Base(path), ".")[0])
			err = os.MkdirAll(covDir, 0755)
			if err != nil {
				log.Fatal(err)
			}
			cmd = fmt.Sprintf(cmd, covDir, r)
			fmt.Println("will run command: ", cmd)
			command := exec.Command("sh", "-c", cmd)
			out, err := command.CombinedOutput()
			os.WriteFile(filepath.Join(covDir, "output.txt"), out, 0644)
			if err != nil {
				fmt.Println("error running command: ", err)
			}
		}
		return nil
	}
	err = filepath.WalkDir(dir, walkDir)
	if err != nil {
		log.Fatal(err)
	}
}

// FuncVisitor implements the visitor that builds the function position list for a file.
type FuncVisitor struct {
	funcs []string
}

func (v *FuncVisitor) Visit(node ast.Node) ast.Visitor {
	switch n := node.(type) {
	case *ast.FuncDecl:
		if n.Body == nil {
			break
		}
		if n.Type.Params.NumFields() != 1 {
			break
		}
		if n.Recv != nil {
			break
		}
		if n.Name.Name[0] < 'A' || n.Name.Name[0] > 'Z' {
			// Do not count declarations of private functions.
			break
		}
		paramType := types.ExprString(n.Type.Params.List[0].Type)
		if paramType != "*testing.T" {
			break
		}
		v.funcs = append(v.funcs, n.Name.Name)
	}
	return v
}
