package java

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// ClasspathRequest 是 gradle / maven 插件 prepareClasspath 服务的输入。
type ClasspathRequest struct {
	Project    sdk.ProjectInfo          `json:"project"`
	Module     sdk.ModuleInfo           `json:"module"`
	Toolchains map[string]sdk.Toolchain `json:"toolchains,omitempty"`
	Env        map[string]string        `json:"env,omitempty"`
	RunID      string                   `json:"runId,omitempty"`
	Subproject string                   `json:"subproject,omitempty"` // Gradle 路径 / Maven 模块，空为根
	SourceSet  string                   `json:"sourceSet,omitempty"`
}

// ClasspathResult 是 prepareClasspath 服务的输出。
type ClasspathResult struct {
	Classpath  []string `json:"classpath"`
	MainClass  string   `json:"mainClass,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"` // 子工程目录，作为默认工作目录
}

// PrepareRun 处理 java-application：交给构建插件编译并导出类路径，再直接用 java 启动主类。
func (p *Plugin) PrepareRun(ctx *sdk.Context, rp *sdk.PrepareRunParams) (*sdk.LaunchSpec, error) {
	if rp.Config.Type != typeApplication {
		return nil, sdk.ErrNotSupported
	}
	opts := rp.Config.Options
	main := sdk.OptString(opts, "mainClass", "")
	if main == "" {
		return nil, sdk.UserError("%s", ctx.T("err.noMainClass"))
	}
	jdk, ok := rp.Toolchains["jdk"]
	if !ok || jdk.Path == "" {
		return nil, sdk.UserError("%s", ctx.T("err.noJdk"))
	}
	cpRes, err := p.resolveClasspath(ctx, rp, sdk.OptString(opts, "module", ""))
	if err != nil {
		return nil, err
	}
	cwd := resolveWorkDir(sdk.OptString(opts, "workingDir", ""), rp.Project.Root, firstNonEmpty(cpRes.WorkingDir, rp.Module.Dir))
	return buildCommand(p.cacheDir(ctx), &BuildCommandParams{
		JDK:         jdk,
		Classpath:   cpRes.Classpath,
		MainClass:   main,
		VMOptions:   sdk.OptString(opts, "vmOptions", ""),
		ProgramArgs: sdk.OptString(opts, "programArgs", ""),
		Cwd:         cwd,
		Key:         firstNonEmpty(rp.Config.ID, sdk.StableID(main)),
	})
}

// resolveClasspath 按构建工具取类路径：Gradle/Maven 走对应插件的 prepareClasspath 服务，纯 Java 自己用 javac 编译。
func (p *Plugin) resolveClasspath(ctx *sdk.Context, rp *sdk.PrepareRunParams, subproject string) (*ClasspathResult, error) {
	tool := buildTool(&rp.Module)
	if tool == "gradle" || tool == "maven" {
		var res ClasspathResult
		req := &ClasspathRequest{Project: rp.Project, Module: rp.Module, Toolchains: rp.Toolchains, Env: rp.Env, RunID: rp.RunID, Subproject: subproject}
		if err := ctx.Host.Call(ctx, tool, "prepareClasspath", req, &res); err != nil {
			return nil, err
		}
		return &res, nil
	}
	return p.compilePlain(ctx, rp)
}

// resolveWorkDir 解析工作目录：相对路径按工程根解析，空则用默认目录。
func resolveWorkDir(wd, projectRoot, def string) string {
	if wd == "" {
		return def
	}
	wd = sdk.Expand(wd)
	if !filepath.IsAbs(wd) {
		wd = filepath.Join(projectRoot, wd)
	}
	return wd
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// compilePlain 编译没有构建工具的纯 Java 目录：输出放插件缓存目录，不写工程目录；lib/ libs/ 下的 jar 进类路径。
func (p *Plugin) compilePlain(ctx *sdk.Context, rp *sdk.PrepareRunParams) (*ClasspathResult, error) {
	jdk := rp.Toolchains["jdk"]
	dir := rp.Module.Dir
	srcRoot := dir
	if sdk.DirExists(filepath.Join(dir, "src", "main", "java")) {
		srcRoot = filepath.Join(dir, "src", "main", "java")
	} else if sdk.DirExists(filepath.Join(dir, "src")) {
		srcRoot = filepath.Join(dir, "src")
	}
	var sources, resources []string
	_ = filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != srcRoot && (strings.HasPrefix(d.Name(), ".") || sdk.DefaultSkipDirs[d.Name()] || d.Name() == "test") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".java") {
			sources = append(sources, path)
		} else if srcRoot != dir && !strings.HasPrefix(d.Name(), ".") {
			resources = append(resources, path)
		}
		return nil
	})
	if len(sources) == 0 {
		return nil, sdk.UserError("%s", ctx.T("err.noSources"))
	}
	var libs []string
	for _, l := range []string{"lib", "libs"} {
		m, _ := filepath.Glob(filepath.Join(dir, l, "*.jar"))
		libs = append(libs, m...)
	}
	out := filepath.Join(p.cacheDir(ctx), "plain", sdk.StableID(dir), "classes")
	_ = os.RemoveAll(out)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	argfile := filepath.Join(p.cacheDir(ctx), "plain", sdk.StableID(dir), "javac.argfile")
	args := []string{"-d", out, "-encoding", "UTF-8", "-g"}
	if len(libs) > 0 {
		args = append(args, "-cp", strings.Join(libs, string(os.PathListSeparator)))
	}
	args = append(args, sources...)
	if JavaMajor(jdk.Version) >= 9 || jdk.Version == "" {
		if err := writeArgFile(argfile, args); err != nil {
			return nil, err
		}
		args = []string{"@" + argfile}
	}
	ctx.Host.Console(rp.RunID, ctx.T("console.compiling", len(sources)))
	r, err := ctx.Host.Exec(ctx, &sdk.ExecParams{
		Command:     filepath.Join(jdk.Path, "bin", sdk.ExeName("javac")),
		Args:        args,
		Cwd:         dir,
		Env:         sdk.MergeEnv(rp.Env, jdk.Env),
		PathPrepend: jdk.PathPrepend,
		Console:     rp.RunID,
		Title:       ctx.T("title.javac"),
	})
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, sdk.UserError("%s", ctx.T("err.compileFailed"))
	}
	for _, res := range resources {
		rel, err := filepath.Rel(srcRoot, res)
		if err != nil {
			continue
		}
		_ = copyFile(res, filepath.Join(out, rel))
	}
	return &ClasspathResult{Classpath: append([]string{out}, libs...), WorkingDir: dir}, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
