//go:build linux && (amd64 || arm64)

package usb

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// Stack moves are timing-dependent, so guard the compiler-recognized pointer
// escape contract in the checkout, then compile/vet the Windows target too.
// Native WIN runs prebuilt binaries and does not receive source files.
func TestWindowsProcPointerEscapeContract(t *testing.T) {
	var source []byte
	var err error
	for _, path := range []string{"backend_windows.go", "internal/installer/usb/backend_windows.go"} {
		source, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "backend_windows.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "winProc" {
			continue
		}
		if fn.Doc != nil {
			for _, comment := range fn.Doc.List {
				if comment.Text == "//go:uintptrescapes" {
					return
				}
			}
		}
		t.Fatal("winProc must declare //go:uintptrescapes so live Go pointers survive stack moves before the syscall")
	}
	t.Fatal("WinUSB syscall wrapper missing; explicitly verify its replacement pointer-escape contract")
}
