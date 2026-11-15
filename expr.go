package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"
)

var binaryOps = map[token.Token]bool{
	token.ADD: true, token.SUB: true, token.MUL: true, token.QUO: true,
	token.EQL: true, token.NEQ: true, token.LSS: true, token.LEQ: true, token.GTR: true, token.GEQ: true,
	token.LAND: true, token.LOR: true,
}

// compile parses src as a Go expression and rejects everything outside the
// small subset eval understands, so a bad expression fails when the pipeline
// is defined rather than on the first row.
func compile(src string) (ast.Expr, error) {
	e, err := parser.ParseExpr(src)
	if err != nil {
		return nil, fmt.Errorf("bad expression %q: %v", src, err)
	}
	ok := true
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case nil, *ast.Ident, *ast.ParenExpr:
		case *ast.BasicLit:
			ok = ok && (n.Kind == token.INT || n.Kind == token.FLOAT || n.Kind == token.STRING)
		case *ast.UnaryExpr:
			ok = ok && (n.Op == token.SUB || n.Op == token.NOT)
		case *ast.BinaryExpr:
			ok = ok && binaryOps[n.Op]
		default:
			ok = false
		}
		return ok
	})
	if !ok {
		return nil, fmt.Errorf("bad expression %q: unsupported syntax", src)
	}
	return e, nil
}

func eval(e ast.Expr, f map[string]any) (any, error) {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return eval(e.X, f)
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return strconv.Unquote(e.Value)
		}
		return strconv.ParseFloat(e.Value, 64)
	case *ast.Ident:
		switch e.Name {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		v, ok := f[e.Name]
		if !ok {
			return nil, fmt.Errorf("unknown field %q", e.Name)
		}
		return v, nil
	case *ast.UnaryExpr:
		if e.Op == token.NOT {
			b, err := evalBool(e.X, f)
			return !b, err
		}
		n, err := evalNum(e.X, f)
		return -n, err
	case *ast.BinaryExpr:
		return evalBinary(e, f)
	}
	return nil, errors.New("unsupported expression")
}

func evalBinary(e *ast.BinaryExpr, f map[string]any) (any, error) {
	switch e.Op {
	case token.LAND, token.LOR:
		l, err := evalBool(e.X, f)
		if err != nil {
			return nil, err
		}
		if l == (e.Op == token.LOR) {
			return l, nil // short-circuit: false && _, true || _
		}
		return evalBool(e.Y, f)
	case token.EQL, token.NEQ:
		l, err := eval(e.X, f)
		if err != nil {
			return nil, err
		}
		r, err := eval(e.Y, f)
		if err != nil {
			return nil, err
		}
		equal := fmt.Sprint(l) == fmt.Sprint(r)
		if ln, ok := toNum(l); ok {
			if rn, ok := toNum(r); ok {
				equal = ln == rn
			}
		}
		return equal == (e.Op == token.EQL), nil
	}

	l, err := evalNum(e.X, f)
	if err != nil {
		return nil, err
	}
	r, err := evalNum(e.Y, f)
	if err != nil {
		return nil, err
	}
	switch e.Op {
	case token.LSS:
		return l < r, nil
	case token.LEQ:
		return l <= r, nil
	case token.GTR:
		return l > r, nil
	case token.GEQ:
		return l >= r, nil
	}

	var n float64
	switch e.Op {
	case token.ADD:
		n = l + r
	case token.SUB:
		n = l - r
	case token.MUL:
		n = l * r
	case token.QUO:
		if r == 0 {
			return nil, errors.New("division by zero")
		}
		n = l / r
	}
	// JSON cannot carry Inf, so an overflow must reject the row here.
	if math.IsInf(n, 0) {
		return nil, errors.New("result is not a finite number")
	}
	return n, nil
}

func evalBool(e ast.Expr, f map[string]any) (bool, error) {
	v, err := eval(e, f)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, errors.New("expression is not true or false")
	}
	return b, nil
}

func evalNum(e ast.Expr, f map[string]any) (float64, error) {
	v, err := eval(e, f)
	if err != nil {
		return 0, err
	}
	if n, ok := toNum(v); ok {
		return n, nil
	}
	if id, ok := e.(*ast.Ident); ok {
		return 0, fmt.Errorf("%s: %#v is not a number", id.Name, v)
	}
	return 0, fmt.Errorf("%#v is not a number", v)
}

// toNum accepts numbers and numeric strings, because CSV and many JSON
// sources carry numbers as text.
func toNum(v any) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
	}
	return 0, false
}
