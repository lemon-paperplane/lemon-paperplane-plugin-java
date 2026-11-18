package java

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

const (
	typeApplication = "java-application"
	maxSourceFiles  = 20000
	maxMainClasses  = 50
	maxFileSize     = 1 << 20
)

var (
	mainMethodRe = regexp.MustCompile(`\b(?:public\s+static|static\s+public|static)\s+(?:final\s+)?void\s+main\s*\(\s*(?:final\s+)?(?:java\.lang\.)?String\b`)
	packageRe    = regexp.MustCompile(`(?m)^\s*package\s+([\w.]+)\s*;`)
	languageRe   = regexp.MustCompile(`languageLevel="JDK_(\d+)(?:_(\d+))?`)
	jdkNameRe    = regexp.MustCompile(`project-jdk-name="([^"]*)"`)
	trailingNum  = regexp.MustCompile(`(\d+)(?:\.\d+)*$`)
)

// MainClass 是识别到的一个可运行主类。
type MainClass struct {
	Class   string `json:"class"`
	Project string `json:"project,omitempty"` // Gradle 子工程路径或 Maven 模块
	File    string `json:"file"`
}

// Model 是 java 插件的模型。
type Model struct {
	BuildTool   string      `json:"buildTool"`
	MainClasses []MainClass `json:"mainClasses,omitempty"`
}

// Analyze 识别主类、导入 IDEA 的 Application 运行配置；纯 Java 模块给出 JDK 需求。
func (p *Plugin) Analyze(ctx *sdk.Context, a *sdk.AnalyzeParams) (*sdk.AnalyzeResult, error) {
	m := &a.Module
	tool := buildTool(m)
	projects, android := moduleProjects(m)
	res := &sdk.AnalyzeResult{}
	model := Model{BuildTool: tool}
	if !android {
		model.MainClasses = scanMainClasses(m.Dir, tool, projects)
	}
	hasBoot := m.HasFacet("spring-boot")

	var configs []sdk.RunConfiguration
	names := map[string]int{}
	for _, mc := range model.MainClasses {
		names[simpleName(mc.Class)]++
	}
	for _, mc := range model.MainClasses {
		name := simpleName(mc.Class)
		if names[name] > 1 && mc.Project != "" && mc.Project != ":" {
			name += " (" + strings.TrimPrefix(mc.Project, ":") + ")"
		}
		opts := map[string]any{"mainClass": mc.Class}
		if mc.Project != "" {
			opts["module"] = mc.Project
		}
		configs = append(configs, sdk.RunConfiguration{
			ID:       sdk.StableID(typeApplication, mc.Project, mc.Class),
			Name:     name,
			Type:     typeApplication,
			Options:  opts,
			Source:   "auto",
			Priority: 10,
		})
	}
	// IDEA 的 Application 运行配置
	rootName := ideaRootName(m)
	for _, c := range a.IdeConfigs {
		if c.Type != "Application" {
			continue
		}
		main := c.Options["MAIN_CLASS_NAME"]
		if main == "" {
			continue
		}
		opts := map[string]any{"mainClass": main}
		if mod := ideaModuleToProject(c.Module, rootName, tool); mod != "" {
			opts["module"] = mod
		}
		if v := c.Options["VM_PARAMETERS"]; v != "" {
			opts["vmOptions"] = v
		}
		if v := c.Options["PROGRAM_PARAMETERS"]; v != "" {
			opts["programArgs"] = v
		}
		if wd := ideaPath(c.Options["WORKING_DIRECTORY"], a.Project.Root, m.Dir); wd != "" {
			opts["workingDir"] = wd
		}
		cfg := sdk.RunConfiguration{
			ID:       sdk.StableID("idea", typeApplication, c.Name, main),
			Name:     c.Name,
			Type:     typeApplication,
			Options:  opts,
			Env:      c.Env,
			Source:   "idea",
			Priority: 20,
		}
		// 同主类的自动配置让位给 IDEA 配置
		kept := configs[:0]
		for _, x := range configs {
			if !(x.Source == "auto" && x.Options["mainClass"] == main) {
				kept = append(kept, x)
			}
		}
		configs = append(kept, cfg)
	}
	if len(configs) == 1 && !hasBoot && !m.HasFacet("kotlin") {
		configs[0].Default = true
	}
	res.RunConfigurations = configs

	if tool == "none" {
		req := sdk.ToolchainRequirement{Kind: "jdk", Constraint: ">=8", Source: "java"}
		if lv := ideaLanguageLevel(a.Project.Root); lv != "" {
			req.Preferred = lv
			req.Source = ".idea/misc.xml"
		}
		res.Requirements = []sdk.ToolchainRequirement{req}
		res.Info = []sdk.InfoItem{{Label: "Java", Value: ctx.T("info.plainJava")}}
		if req.Preferred != "" {
			res.Info = append(res.Info, sdk.InfoItem{Label: "JDK", Value: req.Preferred})
		}
	}
	res.Model = mustJSON(model)
	return res, nil
}

func simpleName(fqcn string) string {
	if i := strings.LastIndexByte(fqcn, '.'); i >= 0 {
		return fqcn[i+1:]
	}
	return fqcn
}

// scanMainClasses 在各（子）工程的 Java 源码目录里找 main 方法。
// 带 @SpringBootApplication 的类交给 spring-boot 插件。
func scanMainClasses(root, tool string, projects []subproject) []MainClass {
	var out []MainClass
	files := 0
	for _, pr := range projects {
		if pr.Android {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(pr.Dir))
		var srcRoots []string
		switch {
		case sdk.DirExists(filepath.Join(dir, "src", "main", "java")):
			srcRoots = []string{filepath.Join(dir, "src", "main", "java")}
		case tool == "none" && sdk.DirExists(filepath.Join(dir, "src")):
			srcRoots = []string{filepath.Join(dir, "src")}
		case tool == "none":
			srcRoots = []string{dir}
		}
		for _, sr := range srcRoots {
			_ = filepath.WalkDir(sr, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					n := d.Name()
					if path != sr && (strings.HasPrefix(n, ".") || sdk.DefaultSkipDirs[n] || n == "test") {
						return fs.SkipDir
					}
					// 纯 Java 目录里嵌套的其他构建工程不扫
					if path != sr && tool == "none" && (sdk.FileExists(filepath.Join(path, "pom.xml")) || sdk.FileExists(filepath.Join(path, "build.gradle")) || sdk.FileExists(filepath.Join(path, "build.gradle.kts"))) {
						return fs.SkipDir
					}
					return nil
				}
				if !strings.HasSuffix(d.Name(), ".java") || d.Name() == "module-info.java" || d.Name() == "package-info.java" {
					return nil
				}
				files++
				if files > maxSourceFiles || len(out) >= maxMainClasses {
					return fs.SkipAll
				}
				if cls := mainClassOf(path); cls != "" {
					out = append(out, MainClass{Class: cls, Project: pr.Path, File: path})
				}
				return nil
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// mainClassOf 读取一个 .java 文件，有 main 方法则返回全限定类名（包名 + 文件名）。
func mainClassOf(path string) string {
	st, err := os.Stat(path)
	if err != nil || st.Size() > maxFileSize {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return mainClassFromSource(data, strings.TrimSuffix(filepath.Base(path), ".java"))
}

func mainClassFromSource(data []byte, className string) string {
	if !bytes.Contains(data, []byte("main")) || !bytes.Contains(data, []byte("static")) {
		return ""
	}
	if !mainMethodRe.Match(data) {
		return ""
	}
	if bytes.Contains(data, []byte("@SpringBootApplication")) {
		return ""
	}
	if m := packageRe.FindSubmatch(data); m != nil {
		return string(m[1]) + "." + className
	}
	return className
}

// ---------------------------------------------------------------------------
// IDEA 配置

// ideaRootName 返回 IDEA 模块名前缀（Gradle 根工程名）。
func ideaRootName(m *sdk.ModuleInfo) string {
	var data buildFacetData
	if facetData(m, "gradle", &data) && data.RootName != "" {
		return data.RootName
	}
	return m.Name
}

// ideaModuleToProject 把 IDEA 模块名转成子工程标识：
// Gradle 「vrtalk-api.vrtalk-api-starter.main」→「:vrtalk-api-starter」；Maven 模块名就是 artifactId。
func ideaModuleToProject(module, rootName, tool string) string {
	if module == "" {
		return ""
	}
	if tool != "gradle" {
		return module
	}
	for _, suf := range []string{".main", ".test", ".testFixtures", ".integrationTest"} {
		if strings.HasSuffix(module, suf) {
			module = strings.TrimSuffix(module, suf)
			break
		}
	}
	if module == rootName {
		return ":"
	}
	if rootName != "" && strings.HasPrefix(module, rootName+".") {
		module = strings.TrimPrefix(module, rootName+".")
	}
	return ":" + strings.ReplaceAll(module, ".", ":")
}

// ideaPath 展开 IDEA 路径宏；模块目录宏返回空（表示默认的模块目录）。
func ideaPath(p, projectRoot, moduleDir string) string {
	if p == "" || strings.Contains(p, "$MODULE_WORKING_DIR$") || strings.Contains(p, "$MODULE_DIR$") {
		return ""
	}
	p = strings.ReplaceAll(p, "$PROJECT_DIR$", projectRoot)
	p = strings.ReplaceAll(p, "$USER_HOME$", sdk.HomeDir())
	return filepath.Clean(p)
}

// ideaLanguageLevel 从 .idea/misc.xml 读工程语言级别 / 工程 JDK 名称，返回主版本号。
func ideaLanguageLevel(root string) string {
	data := sdk.ReadText(filepath.Join(root, ".idea", "misc.xml"), 256*1024)
	if data == "" {
		return ""
	}
	if m := languageRe.FindStringSubmatch(data); m != nil {
		if m[1] == "1" && m[2] != "" {
			return m[2]
		}
		return m[1]
	}
	if m := jdkNameRe.FindStringSubmatch(data); m != nil {
		if n := trailingNum.FindStringSubmatch(m[1]); n != nil {
			if mj := JavaMajor(n[0]); mj > 0 {
				return itoa(mj)
			}
		}
	}
	return ""
}
