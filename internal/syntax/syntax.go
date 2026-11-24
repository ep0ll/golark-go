// Package syntax adapts the standard Go front-end for Golark source files.
package syntax

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
)

// Token is a lexical Go token with its source position and literal spelling.
type Token struct {
	Position token.Position
	Kind     token.Token
	Literal  string
}

// ParseFile parses one complete Go source file. Go's parser is deliberately
// used without a parallel lexer or grammar so Golark stays aligned with Go.
func ParseFile(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
	if fset == nil {
		return nil, fmt.Errorf("golark: nil token.FileSet")
	}
	if filename == "" {
		filename = "<golark>"
	}
	return parser.ParseFile(fset, filename, src, parser.AllErrors|parser.ParseComments|parser.SkipObjectResolution)
}

// ParseExpr parses a single Go expression.
func ParseExpr(fset *token.FileSet, filename string, src []byte) (ast.Expr, error) {
	if fset == nil {
		return nil, fmt.Errorf("golark: nil token.FileSet")
	}
	return parser.ParseExprFrom(fset, filename, src, parser.AllErrors|parser.SkipObjectResolution)
}

// Lex scans Go source with go/scanner. Comments are included in the token
// stream, and scanner diagnostics are returned together with any tokens read.
func Lex(filename string, src []byte) ([]Token, error) {
	if filename == "" {
		filename = "<golark>"
	}
	fset := token.NewFileSet()
	file := fset.AddFile(filename, fset.Base(), len(src))
	var s scanner.Scanner
	var scanErr error
	s.Init(file, src, func(pos token.Position, msg string) {
		if scanErr == nil {
			scanErr = fmt.Errorf("%s: %s", pos, msg)
		}
	}, scanner.ScanComments)
	out := make([]Token, 0)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		out = append(out, Token{Position: fset.Position(pos), Kind: tok, Literal: lit})
	}
	return out, scanErr
}

// Format returns canonical Go formatting for a syntax tree.
func Format(fset *token.FileSet, node any) ([]byte, error) {
	if fset == nil {
		fset = token.NewFileSet()
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, node); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// FormatSource parses and formats Go source.
func FormatSource(filename string, src []byte) ([]byte, error) {
	_ = filename // retained for API symmetry with ParseFile
	return format.Source(src)
}
