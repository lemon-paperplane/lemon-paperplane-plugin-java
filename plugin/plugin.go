// Package java 是 Lemon Paperplane 的 Java 插件：JDK 工具链（发现 / 下载 Temurin）、Java facet、
// java-application 运行配置，以及供其他 JVM 插件调用的 buildCommand 服务。
package java

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

var (
	runtimeGOOS   = runtime.GOOS
	runtimeGOARCH = runtime.GOARCH
)

// Plugin 实现 java 插件。
type Plugin struct{}

// New 返回插件实例。
func New() any { return &Plugin{} }

func (p *Plugin) cacheDir(ctx *sdk.Context) string {
	if ctx.Init != nil && ctx.Init.CacheDir != "" {
		return ctx.Init.CacheDir
	}
	return filepath.Join(os.TempDir(), "paperplane-java")
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func itoa(n int) string { return strconv.Itoa(n) }

// ---------------------------------------------------------------------------
// 工具链

// DiscoverToolchains 扫描本机 JDK。
func (p *Plugin) DiscoverToolchains(ctx *sdk.Context, d *sdk.DiscoverToolchainsParams) (*sdk.DiscoverToolchainsResult, error) {
	if d.Kind != "jdk" {
		return nil, sdk.ErrNotSupported
	}
	return &sdk.DiscoverToolchainsResult{Toolchains: discoverJDKs(ctx.Init, d.Paths)}, nil
}

// AvailableToolchains 列出可下载的 Temurin 版本。
func (p *Plugin) AvailableToolchains(ctx *sdk.Context, a *sdk.AvailableToolchainsParams) (*sdk.AvailableToolchainsResult, error) {
	if a.Kind != "jdk" {
		return nil, sdk.ErrNotSupported
	}
	return &sdk.AvailableToolchainsResult{Versions: p.availableVersions(ctx)}, nil
}

// InstallToolchain 返回 Temurin 下载计划，由内核下载解压。
func (p *Plugin) InstallToolchain(ctx *sdk.Context, in *sdk.InstallToolchainParams) (*sdk.InstallToolchainResult, error) {
	if in.Kind != "jdk" {
		return nil, sdk.ErrNotSupported
	}
	plan, err := p.installPlan(ctx, in.Version)
	if err != nil {
		return nil, err
	}
	return &sdk.InstallToolchainResult{Plan: plan}, nil
}

// ---------------------------------------------------------------------------
// 服务

// CallService 处理 buildCommand。
func (p *Plugin) CallService(ctx *sdk.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "buildCommand":
		var in BuildCommandParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, sdk.NewError(sdk.ErrCodeInvalidParams, "%v", err)
		}
		if in.JDK.Path == "" && in.JDK.Executable == "" {
			return nil, sdk.UserError("%s", ctx.T("err.noJdk"))
		}
		if in.MainClass == "" {
			return nil, sdk.UserError("%s", ctx.T("err.noMainClass"))
		}
		return buildCommand(p.cacheDir(ctx), &in)
	}
	return nil, sdk.ErrNotSupported
}

// ---------------------------------------------------------------------------
// 修复

var (
	xmxRe       = regexp.MustCompile(`-Xmx(\d+)([kKmMgG]?)`)
	metaspaceRe = regexp.MustCompile(`-XX:MaxMetaspaceSize=(\d+)([kKmMgG]?)`)
)

// ApplyFix 处理 custom 修复：加大堆内存 / 元空间（保留用户已有的其他 VM 参数）。
func (p *Plugin) ApplyFix(ctx *sdk.Context, f *sdk.ApplyFixParams) (*sdk.ApplyFixResult, error) {
	vm := ""
	if f.Config != nil {
		vm = sdk.OptString(f.Config.Options, "vmOptions", "")
	}
	switch f.Fix.ID {
	case "increaseHeap":
		nv, size := increaseOption(vm, xmxRe, "-Xmx", defaultHeapMB())
		return &sdk.ApplyFixResult{Changes: &sdk.ProjectChanges{RunOptions: map[string]any{"vmOptions": nv}}, Rerun: true, Message: ctx.T("fix.heapSet", size)}, nil
	case "increaseMetaspace":
		nv, size := increaseOption(vm, metaspaceRe, "-XX:MaxMetaspaceSize=", 512)
		return &sdk.ApplyFixResult{Changes: &sdk.ProjectChanges{RunOptions: map[string]any{"vmOptions": nv}}, Rerun: true, Message: ctx.T("fix.metaspaceSet", size)}, nil
	}
	return nil, sdk.ErrNotSupported
}

// increaseOption 把 VM 参数里的某个内存选项翻倍（没有则设为 defMB），返回新参数串与新值。
func increaseOption(vm string, re *regexp.Regexp, prefix string, defMB int) (string, string) {
	mb := defMB
	if m := re.FindStringSubmatch(vm); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch strings.ToLower(m[2]) {
		case "g":
			n *= 1024
		case "k":
			n /= 1024
		case "":
			n /= 1024 * 1024
		}
		if n*2 > mb {
			mb = n * 2
		}
		vm = strings.TrimSpace(re.ReplaceAllString(vm, ""))
	}
	size := strconv.Itoa(mb) + "m"
	if mb%1024 == 0 {
		size = strconv.Itoa(mb/1024) + "g"
	}
	return strings.TrimSpace(strings.Join(strings.Fields(vm+" "+prefix+size), " ")), size
}

// defaultHeapMB：物理内存的一半（2–8 GB 之间），拿不到时 4 GB。
func defaultHeapMB() int {
	total := physicalMemoryMB()
	if total <= 0 {
		return 4096
	}
	h := total / 2 / 1024 * 1024
	if h < 2048 {
		h = 2048
	}
	if h > 8192 {
		h = 8192
	}
	return h
}

func physicalMemoryMB() int {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			return int(n / 1024 / 1024)
		}
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err == nil {
			if m := regexp.MustCompile(`MemTotal:\s+(\d+) kB`).FindSubmatch(data); m != nil {
				n, _ := strconv.Atoi(string(m[1]))
				return n / 1024
			}
		}
	}
	return 0
}
