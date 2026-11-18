package java

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

var buildFiles = []string{"pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"}
var gradleFiles = []string{"build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"}

// Detect 识别 Java 模块：Gradle/Maven 工程里有 Java 源码目录，或没有构建工具的纯 Java 目录。
func (p *Plugin) Detect(ctx *sdk.Context, d *sdk.DetectParams) (*sdk.DetectResult, error) {
	if inSourceTree(d.RelDir) {
		return &sdk.DetectResult{}, nil
	}
	if sdk.HasFile(d.Files, buildFiles...) {
		tool := "maven"
		if sdk.HasFile(d.Files, gradleFiles...) {
			tool = "gradle"
		}
		if !hasJavaSourceRoots(d.Dir) {
			return &sdk.DetectResult{}, nil
		}
		return &sdk.DetectResult{
			Facets: []sdk.Facet{{ID: "java", Data: map[string]any{"buildTool": tool}}},
			Claims: []string{"src"},
		}, nil
	}
	if sdk.HasFile(d.Files, "*.java") || (sdk.HasFile(d.Files, "src/") && dirHasJava(filepath.Join(d.Dir, "src"), 6)) {
		return &sdk.DetectResult{
			Facets: []sdk.Facet{{ID: "java", Data: map[string]any{"buildTool": "none"}}},
			Claims: []string{"src", "lib", "libs", "out"},
		}, nil
	}
	return &sdk.DetectResult{}, nil
}

// inSourceTree 判断目录是否位于某个 src 源码树里（这些目录不单独成模块）。
func inSourceTree(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "src" {
			return true
		}
	}
	return false
}

// hasJavaSourceRoots 在构建工程根、一级与二级子目录里找 src/main/java 或 src/test/java。
func hasJavaSourceRoots(dir string) bool {
	check := func(d string) bool {
		return sdk.DirExists(filepath.Join(d, "src", "main", "java")) || sdk.DirExists(filepath.Join(d, "src", "test", "java"))
	}
	if check(dir) {
		return true
	}
	budget := 400
	var walk func(d string, depth int) bool
	walk = func(d string, depth int) bool {
		entries, err := os.ReadDir(d)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if budget <= 0 {
				return false
			}
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || sdk.DefaultSkipDirs[e.Name()] || e.Name() == "src" {
				continue
			}
			budget--
			sub := filepath.Join(d, e.Name())
			if check(sub) {
				return true
			}
			if depth < 2 && walk(sub, depth+1) {
				return true
			}
		}
		return false
	}
	return walk(dir, 1)
}

// dirHasJava 在目录下（限定深度）找 .java 文件。
func dirHasJava(dir string, maxDepth int) bool {
	found := false
	budget := 3000
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			if found || budget <= 0 {
				return
			}
			budget--
			if e.IsDir() {
				if depth < maxDepth && !strings.HasPrefix(e.Name(), ".") && !sdk.DefaultSkipDirs[e.Name()] {
					walk(filepath.Join(d, e.Name()), depth+1)
				}
				continue
			}
			if strings.HasSuffix(e.Name(), ".java") {
				found = true
				return
			}
		}
	}
	walk(dir, 0)
	return found
}

// ---------------------------------------------------------------------------
// 读取构建插件在 facet 里留下的子工程清单

// subproject 是 Gradle 子工程或 Maven 模块。
type subproject struct {
	Path    string `json:"path"` // Gradle ":a:b"；Maven artifactId；纯 Java 为空
	Dir     string `json:"dir"`  // 相对模块根
	Android bool   `json:"android,omitempty"`
}

type buildFacetData struct {
	Android  bool         `json:"android"`
	RootName string       `json:"rootName"`
	Projects []subproject `json:"projects"`
}

func facetData(m *sdk.ModuleInfo, id string, v any) bool {
	f := m.Facet(id)
	if f == nil || f.Data == nil {
		return false
	}
	b, err := json.Marshal(f.Data)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// buildTool 返回模块的构建工具：gradle | maven | none。
func buildTool(m *sdk.ModuleInfo) string {
	switch {
	case m.HasFacet("gradle"):
		return "gradle"
	case m.HasFacet("maven"):
		return "maven"
	}
	return "none"
}

// moduleProjects 返回模块里的（子）工程清单与是否 Android。
func moduleProjects(m *sdk.ModuleInfo) ([]subproject, bool) {
	var data buildFacetData
	switch buildTool(m) {
	case "gradle":
		facetData(m, "gradle", &data)
		if len(data.Projects) == 0 {
			data.Projects = []subproject{{Path: ":", Dir: ""}}
		}
	case "maven":
		facetData(m, "maven", &data)
		if len(data.Projects) == 0 {
			data.Projects = []subproject{{Path: "", Dir: ""}}
		}
	default:
		data.Projects = []subproject{{Path: "", Dir: ""}}
	}
	return data.Projects, data.Android
}
