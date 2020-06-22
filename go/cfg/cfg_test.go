// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cfg

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"strings"
	"testing"
)

const src = `package main

import "log"

func f1() {
	live()
	return
	dead()
}

func f2() {
	for {
		live()
	}
	dead()
}

func f3() {
	if true { // even known values are ignored
		return
	}
	for true { // even known values are ignored
		live()
	}
	for {
		live()
	}
	dead()
}

func f4(x int) {
	switch x {
	case 1:
		live()
		fallthrough
	case 2:
		live()
		log.Fatal()
	default:
		panic("oops")
	}
	dead()
}

func f5(ch chan int) {
	select {
	case <-ch:
		live()
		return
	default:
		live()
		panic("oops")
	}
	dead()
}

func f6(unknown bool) {
	for {
		if unknown {
			break
		}

		continue
		dead()
	}
	live()
}

func f7(unknown bool) {
outer:
	for {
		for {
			break outer
			dead()
		}
		dead()
	}
	live()
}

func f8() {
	for {
		break nosuchlabel
		dead()
	}
	dead()
}

func f9() {
	select{}
	dead()
}

func f10(ch chan int) {
	select {
	case <-ch:
		return
	}
	dead()
}

func f11(ch chan int) {
	select {
	case <-ch:
		return
		dead()
	default:
	}
	live()
}

func f12() {
	goto; // mustn't crash
	dead()
}

func f13(a int) int {
	if a == 3 {		
	} else {
		return 123
		dead()
	}
	live()
}

func f14(a int) int {
	if a == 3 {
		return a * 5
	} else {
		return 123
	}
	dead()
}

func f15(a int) int {
	if a == 1 {
		return a * 1
	} else if a == 2 {
		return a * 2
	} else if a == 3 {
		return a * 3
	} else if a == 3 {
		return a * 4
	} else {
		if a % 2 == 0 {
			return a / 2
		}
		live()
		return 0
	}

	dead()
	return 100
}
`

func TestDeadCode(t *testing.T) {
	// We'll use dead code detection to verify the CFG.

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "dummy.go", src, parser.Mode(0))
	if err != nil {
		t.Fatal(err)
	}

	for _, decl := range f.Decls {
		if decl, ok := decl.(*ast.FuncDecl); ok {
			g := New(decl.Body, mayReturn)

			var dotGraph bytes.Buffer
			printCFG(&dotGraph, g)

			// Print statements in unreachable blocks
			// (in order determined by builder).
			var buf bytes.Buffer
			for _, b := range g.Blocks {
				if !b.Live {
					for _, n := range b.Nodes {
						fmt.Fprintf(&buf, "\t%s\n", formatNode(fset, n))
					}
				}
			}

			// Check that the result contains "dead" at least once but not "live".
			if !bytes.Contains(buf.Bytes(), []byte("dead")) ||
				bytes.Contains(buf.Bytes(), []byte("live")) {
				t.Logf("func name: %s", decl.Name)
				t.Errorf("unexpected dead statements in function %s:\n%s", decl.Name.Name, &buf)
				t.Logf("control flow graph dot format:\n%s", &dotGraph)
				t.Logf("control flow graph:\n%s", g.Format(fset))
			}
		}
	}
}

// A trivial mayReturn predicate that looks only at syntax, not types.
func mayReturn(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name != "panic"
	case *ast.SelectorExpr:
		return fun.Sel.Name != "Fatal"
	}
	return true
}

// PrintCFG print control flow graph.
func printCFG(w io.Writer, graph *CFG) {
	if graph == nil {
		return
	}

	fset := token.NewFileSet()

	fmt.Fprintln(w, "digraph structs {")
	fmt.Fprintln(w, "\tnode [shape=Mrecord]")

	// output nodes

	for _, b := range graph.Blocks {
		var labels []string

		labels = append(labels, fmt.Sprintf("<name> %s", b.String()))

		var codes []string
		for _, n := range b.Nodes {
			codes = append(codes, formatNode(fset, n))
		}
		fmt.Fprintf(w, "\t"+`%d [label=%q tooltip=%q]`+"\n",
			b.Index,
			strings.Join(labels, "|"),
			strings.Join(codes, "\n"),
		)
	}
	for _, b := range graph.Blocks {
		for _, s := range b.Succs {
			fmt.Fprintf(w, "\t"+`%d -> %d`+"\n", b.Index, s.Index)
		}
	}

	// output edges
	fmt.Fprintln(w, "}")
}
