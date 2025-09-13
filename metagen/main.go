package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/meloshub/meloshub/adapter"
	"golang.org/x/tools/go/packages"
	"gopkg.in/yaml.v3"
)

func main() {
	outputFile := flag.String("output", "adapters.yaml", "Path to the output YAML file")
	flag.Parse()

	rootDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Error getting working directory: %v", err)
	}

	log.Println("Starting metadata scan in:", rootDir)

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  rootDir,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		log.Fatalf("Error loading packages: %v", err)
	}

	var allMetadata []adapter.Metadata

	for _, pkg := range pkgs {
		if isIrrelevantPackage(pkg) {
			continue
		}

		if meta := findMetadataInPackage(pkg); meta != nil {
			allMetadata = append(allMetadata, *meta)
			log.Printf("Found metadata for adapter: %s", meta.Id)
		}
	}

	// 在写入文件前进行冲突检查
	if err := checkConflicts(allMetadata, *outputFile); err != nil {
		// 如果发生冲突则报错，且CI将会失败
		log.Fatalf("Conflict check failed: %v", err)
	}
	log.Println("Conflict check passed.")

	sort.Slice(allMetadata, func(i, j int) bool {
		return allMetadata[i].Id < allMetadata[j].Id
	})

	yamlData, err := yaml.Marshal(allMetadata)
	if err != nil {
		log.Fatalf("Error marshalling to YAML: %v", err)
	}

	err = os.WriteFile(*outputFile, yamlData, 0644)
	if err != nil {
		log.Fatalf("Error writing output file: %v", err)
	}

	log.Printf("Successfully generated metadata for %d adapters into %s", len(allMetadata), *outputFile)
}

// checkConflicts 检查新生成的元数据与旧数据是否存在冲突
func checkConflicts(newMetadata []adapter.Metadata, filePath string) error {
	_, err := os.Stat(filePath)
	// 如果文件不存在的话则不用检查冲突
	if errors.Is(err, os.ErrNotExist) {
		log.Println("No existing adapters.yaml file found, skipping conflict check.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not stat existing file %s: %w", filePath, err)
	}

	existingData, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("could not read existing file %s: %w", filePath, err)
	}

	// 解析旧的元数据
	var existingMetadata []adapter.Metadata
	if err := yaml.Unmarshal(existingData, &existingMetadata); err != nil {
		return fmt.Errorf("could not parse existing yaml file %s: %w", filePath, err)
	}

	existingIdSet := make(map[string]bool)
	for _, meta := range existingMetadata {
		existingIdSet[meta.Id] = true
	}

	newIdSet := make(map[string]bool)
	for _, meta := range newMetadata {
		// 检查适配器元数据 ID 冲突
		if existingIdSet[meta.Id] {
		}

		// 检查本次扫描内部是否有重复ID
		if newIdSet[meta.Id] {
			return fmt.Errorf("duplicate adapter Id '%s' found in the current scan", meta.Id)
		}
		newIdSet[meta.Id] = true
	}

	return nil
}

// isIrrelevantPackage 过滤无需扫描的包
func isIrrelevantPackage(pkg *packages.Package) bool {
	return strings.Contains(pkg.PkgPath, "/tools") || len(pkg.GoFiles) == 0
}

// findMetadataInPackage 遍历包中的所有文件，寻找元数据
func findMetadataInPackage(pkg *packages.Package) *adapter.Metadata {
	for _, file := range pkg.Syntax {
		if meta := findMetadataInFile(pkg, file); meta != nil {
			return meta
		}
	}
	return nil
}

// findMetadataInFile 找到模块的init 函数，并从中追踪 Register 调用
func findMetadataInFile(pkg *packages.Package, file *ast.File) *adapter.Metadata {
	var foundMeta *adapter.Metadata

	ast.Inspect(file, func(n ast.Node) bool {
		initFunc, ok := n.(*ast.FuncDecl)
		if !ok || initFunc.Name.Name != "init" {
			return true
		}

		registerArg := findRegisterCallArgument(pkg.TypesInfo, initFunc.Body)
		if registerArg == nil {
			return false // 没有 Register 调用，一般不会出现这种情况，因为注册适配器是必要的
		}

		constructorFunc := findConstructorFunc(pkg.TypesInfo, file, registerArg)
		if constructorFunc == nil {
			log.Printf("Warning: Found adapter.Register call in %s, but could not trace its constructor function.", pkg.Fset.File(file.Pos()).Name())
			return false
		}

		meta := findMetadataInFuncBody(pkg.TypesInfo, constructorFunc.Body)
		if meta != nil {
			foundMeta = meta
		}

		return false // 已处理此 init 函数，停止遍历
	})

	return foundMeta
}

// findRegisterCallArgument 在函数体内寻找 adapter.Register 的调用，并返回其第一个参数。
func findRegisterCallArgument(info *types.Info, body *ast.BlockStmt) ast.Expr {
	var argExpr ast.Expr

	ast.Inspect(body, func(n ast.Node) bool {
		callExpr, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		selExpr, ok := callExpr.Fun.(*ast.SelectorExpr)
		if !ok || selExpr.Sel.Name != "Register" {
			return true
		}

		if obj := info.ObjectOf(selExpr.Sel); obj != nil {
			if obj.Pkg() != nil && strings.HasSuffix(obj.Pkg().Path(), "meloshub/adapter") {
				if len(callExpr.Args) > 0 {
					argExpr = callExpr.Args[0]
					return false
				}
			}
		}
		return true
	})

	return argExpr
}

// findConstructorFunc 根据 Register 的参数，找到对应的构造函数 AST。
func findConstructorFunc(info *types.Info, file *ast.File, arg ast.Expr) *ast.FuncDecl {
	var constructorName string

	if call, ok := arg.(*ast.CallExpr); ok {
		if ident, ok := call.Fun.(*ast.Ident); ok {
			constructorName = ident.Name
		}
	}

	if ident, ok := arg.(*ast.Ident); ok {
		obj := info.ObjectOf(ident)
		if obj == nil {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				return true
			}
			if lhsIdent, ok := assign.Lhs[0].(*ast.Ident); ok {
				if info.ObjectOf(lhsIdent) == obj {
					if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
						if funIdent, ok := call.Fun.(*ast.Ident); ok {
							constructorName = funIdent.Name
							return false
						}
					}
				}
			}
			return true
		})
	}

	if constructorName == "" {
		return nil
	}

	var constructorFunc *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		funcDecl, ok := n.(*ast.FuncDecl)
		if ok && funcDecl.Name.Name == constructorName {
			constructorFunc = funcDecl
			return false
		}
		return true
	})

	return constructorFunc
}

// findMetadataInFuncBody 在任意函数体中寻找 adapter.Metadata 的创建实例
func findMetadataInFuncBody(info *types.Info, body *ast.BlockStmt) *adapter.Metadata {
	var foundMeta *adapter.Metadata

	ast.Inspect(body, func(n ast.Node) bool {
		compLit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}

		if typ := info.TypeOf(compLit); typ != nil {
			if strings.HasSuffix(typ.String(), "adapter.Metadata") {
				meta := parseCompositeLit(info, compLit)
				if meta != nil {
					foundMeta = meta
					return false
				}
			}
		}
		return true
	})

	return foundMeta
}

// parseCompositeLit 解析结构体字面量，提取键值对
func parseCompositeLit(info *types.Info, expr ast.Expr) *adapter.Metadata {
	compLit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}

	var meta adapter.Metadata
	for _, el := range compLit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			keyName := fmt.Sprintf("%s", kv.Key)
			value := getExprValue(info, kv.Value)

			switch keyName {
			case "Id":
				meta.Id = value
			case "Title":
				meta.Title = value
			case "Type":
				meta.Type = adapter.AdapterType(value)
			case "Version":
				meta.Version = value
			case "Author":
				meta.Author = value
			case "Description":
				meta.Description = value
			}
		}
	}
	if meta.Id == "" {
		return nil
	}
	return &meta
}

// getExprValue 从 AST 节点中提取常量或字符串字面量的值
func getExprValue(info *types.Info, expr ast.Expr) string {
	if basicLit, ok := expr.(*ast.BasicLit); ok && basicLit.Kind == token.STRING {
		return strings.Trim(basicLit.Value, `"`)
	}

	if ident, ok := expr.(*ast.Ident); ok {
		if obj := info.ObjectOf(ident); obj != nil {
			if cnst, ok := obj.(*types.Const); ok {
				return constant.StringVal(cnst.Val())
			}
		}
	}

	if selExpr, ok := expr.(*ast.SelectorExpr); ok {
		if obj := info.ObjectOf(selExpr.Sel); obj != nil {
			if cnst, ok := obj.(*types.Const); ok {
				return constant.StringVal(cnst.Val())
			}
		}
	}

	return ""
}
