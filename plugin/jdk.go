package java

import (
	"bufio"
	"context"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// jdkCandidate 是一个待检查的 JDK 目录及其来源。
type jdkCandidate struct {
	dir    string
	source string
}

// NormalizeJavaVersion 把 Java 的各种版本写法规范成三段以上的数字版本：
// "1.8.0_492" → "8.0.492"，"1.7.0" → "7.0.0"，"17" → "17.0.0"，"21.0.12.1" 原样，"17.0.2+8" → "17.0.2"。
func NormalizeJavaVersion(s string) string {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `"`))
	if s == "" {
		return ""
	}
	pre := ""
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i:]
		s = s[:i]
	}
	if strings.HasPrefix(s, "1.") {
		// 1.8.0_492 → 8.0.492
		rest := strings.TrimPrefix(s, "1.")
		rest = strings.ReplaceAll(rest, "_", ".")
		s = rest
	}
	s = strings.ReplaceAll(s, "_", ".")
	parts := strings.Split(s, ".")
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	for i, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			parts[i] = "0"
		}
	}
	return strings.Join(parts, ".") + pre
}

// JavaMajor 返回 Java 版本的主版本号（支持 1.8 写法），无法解析返回 0。
func JavaMajor(s string) int {
	v, ok := sdk.ParseVersion(NormalizeJavaVersion(s))
	if !ok {
		return 0
	}
	return v.Major()
}

// parseRelease 解析 JDK 根目录下的 release 文件（KEY="value" 格式）。
func parseRelease(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.HasPrefix(k, "#") {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}

// vendorName 由 release 文件的 IMPLEMENTOR / IMPLEMENTOR_VERSION 推断常用的发行版名称。
func vendorName(rel map[string]string) string {
	impl := rel["IMPLEMENTOR"]
	iv := strings.ToLower(rel["IMPLEMENTOR_VERSION"])
	switch {
	case strings.HasPrefix(iv, "zulu") || strings.Contains(impl, "Azul"):
		return "Zulu"
	case strings.HasPrefix(iv, "corretto") || strings.HasPrefix(impl, "Amazon"):
		return "Corretto"
	case strings.HasPrefix(iv, "temurin") || strings.Contains(impl, "Adoptium"):
		return "Temurin"
	case strings.Contains(impl, "AdoptOpenJDK"):
		return "AdoptOpenJDK"
	case strings.Contains(impl, "Microsoft"):
		return "Microsoft"
	case rel["GRAALVM_VERSION"] != "" || strings.Contains(impl, "GraalVM"):
		return "GraalVM"
	case strings.Contains(impl, "BellSoft"):
		return "Liberica"
	case strings.Contains(impl, "JetBrains"):
		return "JetBrains Runtime"
	case strings.Contains(impl, "SAP"):
		return "SapMachine"
	case strings.Contains(impl, "Alibaba"):
		return "Dragonwell"
	case strings.Contains(impl, "Tencent"):
		return "Kona"
	case strings.Contains(impl, "Huawei"):
		return "BiSheng"
	case strings.Contains(impl, "IBM") || strings.Contains(impl, "International Business Machines"):
		return "Semeru"
	case strings.Contains(impl, "Red Hat"):
		return "Red Hat"
	case strings.Contains(impl, "Homebrew"):
		return "Homebrew"
	case strings.Contains(impl, "Oracle"):
		return "Oracle"
	case impl == "" || impl == "N/A":
		return "OpenJDK"
	}
	return impl
}

func normArch(a string) string {
	switch strings.ToLower(a) {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64", "x64":
		return "amd64"
	case "x86", "i386", "i686":
		return "386"
	}
	return a
}

// javaHomeOf 返回目录对应的 JAVA_HOME（兼容 macOS 的 Contents/Home 布局），不是 JDK 返回 ""。
// 只认带 javac 的 JDK，不认 JRE。
func javaHomeOf(dir string) string {
	for _, d := range []string{dir, filepath.Join(dir, "Contents", "Home")} {
		if sdk.FileExists(filepath.Join(d, "bin", sdk.ExeName("java"))) && sdk.FileExists(filepath.Join(d, "bin", sdk.ExeName("javac"))) {
			return d
		}
	}
	return ""
}

// inspectJDK 读取一个 JAVA_HOME 的版本、厂商与架构。
func inspectJDK(home string) (version, vendor, arch string, ok bool) {
	if f, err := os.Open(filepath.Join(home, "release")); err == nil {
		rel := parseRelease(f)
		f.Close()
		if v := rel["JAVA_VERSION"]; v != "" {
			return NormalizeJavaVersion(v), vendorName(rel), normArch(rel["OS_ARCH"]), true
		}
	}
	// 没有 release 文件的老 JDK：跑一次 java -XshowSettings:properties -version
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, filepath.Join(home, "bin", sdk.ExeName("java")), "-XshowSettings:properties", "-version").CombinedOutput()
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(line), " = ")
		if found {
			props[k] = v
		}
	}
	v := props["java.version"]
	if v == "" {
		if m := regexp.MustCompile(`version "([^"]+)"`).FindStringSubmatch(string(out)); m != nil {
			v = m[1]
		}
	}
	if v == "" {
		return "", "", "", false
	}
	vendor = vendorName(map[string]string{"IMPLEMENTOR": props["java.vendor"]})
	return NormalizeJavaVersion(v), vendor, normArch(props["os.arch"]), true
}

// ---------------------------------------------------------------------------
// 发现

// discoverJDKs 扫描已知安装位置。paths 非空时只检查给定目录。
func discoverJDKs(init *sdk.InitializeParams, paths []string) []sdk.Toolchain {
	var cands []jdkCandidate
	managedRoot := ""
	if init != nil && init.HostDataDir != "" {
		managedRoot = filepath.Join(init.HostDataDir, "toolchains")
	}
	if len(paths) > 0 {
		for _, p := range paths {
			cands = append(cands, jdkCandidate{dir: sdk.Expand(p), source: sourceOf(p, managedRoot)})
		}
	} else {
		cands = defaultCandidates(managedRoot)
	}
	seen := map[string]bool{}
	var out []sdk.Toolchain
	for _, c := range cands {
		home := javaHomeOf(c.dir)
		if home == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(home)
		if err != nil {
			real = home
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		version, vendor, arch, ok := inspectJDK(home)
		if !ok {
			continue
		}
		if c.source == "" {
			c.source = sourceOf(home, managedRoot)
		}
		tc := sdk.Toolchain{
			Kind:        "jdk",
			Version:     version,
			Vendor:      vendor,
			Arch:        arch,
			Path:        home,
			Executable:  filepath.Join(home, "bin", sdk.ExeName("java")),
			Env:         map[string]string{"JAVA_HOME": home},
			PathPrepend: []string{filepath.Join(home, "bin")},
			Source:      c.source,
			Managed:     c.source == "managed",
			Label:       vendor + " " + version,
		}
		out = append(out, tc)
	}
	sort.SliceStable(out, func(i, j int) bool { return sdk.CompareVersions(out[i].Version, out[j].Version) > 0 })
	return out
}

// sourceOf 根据路径推断来源。
func sourceOf(p, managedRoot string) string {
	home := sdk.HomeDir()
	p = filepath.Clean(sdk.Expand(p))
	has := func(prefix string) bool {
		return prefix != "" && strings.HasPrefix(p+string(filepath.Separator), filepath.Clean(prefix)+string(filepath.Separator))
	}
	switch {
	case has(managedRoot):
		return "managed"
	case has(filepath.Join(home, "Library", "Java", "JavaVirtualMachines")), has(filepath.Join(home, ".jdks")):
		return "ide"
	case has(filepath.Join(home, ".sdkman")):
		return "sdkman"
	case has(filepath.Join(home, ".gradle", "jdks")):
		return "gradle"
	case has("/opt/homebrew"), has("/usr/local/opt"), has("/usr/local/Cellar"), has("/home/linuxbrew/.linuxbrew"):
		return "homebrew"
	}
	return "system"
}

// defaultCandidates 列出所有已知位置的候选目录（按来源优先级排序，去重时靠前的来源生效）。
func defaultCandidates(managedRoot string) []jdkCandidate {
	home := sdk.HomeDir()
	var out []jdkCandidate
	add := func(source string, dirs ...string) {
		for _, d := range dirs {
			out = append(out, jdkCandidate{dir: d, source: source})
		}
	}
	children := func(dir string) []string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var r []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if st, err := os.Stat(full); err == nil && st.IsDir() {
				r = append(r, full)
			}
		}
		return r
	}
	// 两层：<root>/<x> 与 <root>/<x>/<y>（~/.gradle/jdks、托管目录解压后常多一层）
	twoLevels := func(dir string) []string {
		var r []string
		for _, c := range children(dir) {
			r = append(r, c)
			if javaHomeOf(c) == "" {
				r = append(r, children(c)...)
			}
		}
		return r
	}
	if managedRoot != "" {
		add("managed", twoLevels(filepath.Join(managedRoot, "jdk"))...)
	}
	add("ide", children(filepath.Join(home, ".jdks"))...)
	add("sdkman", children(filepath.Join(home, ".sdkman", "candidates", "java"))...)
	add("gradle", twoLevels(filepath.Join(home, ".gradle", "jdks"))...)
	switch runtime.GOOS {
	case "darwin":
		add("ide", children(filepath.Join(home, "Library", "Java", "JavaVirtualMachines"))...)
		for _, brew := range []string{"/opt/homebrew", "/usr/local"} {
			for _, d := range globDirs(filepath.Join(brew, "opt", "openjdk*")) {
				add("homebrew", filepath.Join(d, "libexec", "openjdk.jdk"), filepath.Join(d, "libexec"))
			}
			for _, d := range globDirs(filepath.Join(brew, "Cellar", "openjdk*", "*")) {
				add("homebrew", filepath.Join(d, "libexec", "openjdk.jdk"))
			}
		}
		add("system", children("/Library/Java/JavaVirtualMachines")...)
		for _, e := range macJavaHomes() {
			add("", e)
		}
	case "linux":
		for _, d := range globDirs("/home/linuxbrew/.linuxbrew/opt/openjdk*") {
			add("homebrew", filepath.Join(d, "libexec"))
		}
		add("system", children("/usr/lib/jvm")...)
		add("system", children("/usr/java")...)
		add("system", children("/opt/java")...)
		add("system", globDirs("/opt/jdk*")...)
	case "windows":
		for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles(x86)")} {
			if pf == "" {
				continue
			}
			for _, v := range []string{"Java", "Eclipse Adoptium", "Eclipse Foundation", "AdoptOpenJDK", "Zulu", "Microsoft", "Amazon Corretto", "BellSoft", "Semeru", "SapMachine", "RedHat"} {
				add("system", children(filepath.Join(pf, v))...)
			}
		}
	}
	if jh := os.Getenv("JAVA_HOME"); jh != "" {
		add("", jh)
	}
	return out
}

func globDirs(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	var out []string
	for _, d := range m {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// macJavaHomes 调用 /usr/libexec/java_home -X 读取系统登记的 JVM。
func macJavaHomes() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/libexec/java_home", "-X").Output()
	if err != nil {
		return nil
	}
	var homes []string
	for _, d := range parseJavaHomePlist(out) {
		if h := d["JVMHomePath"]; h != "" {
			homes = append(homes, h)
		}
	}
	return homes
}

// parseJavaHomePlist 解析 java_home -X 输出的 plist：<array> 里若干 <dict>，返回每个 dict 的字符串键值。
func parseJavaHomePlist(data []byte) []map[string]string {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	var out []map[string]string
	var cur map[string]string
	key := ""
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "dict":
				depth++
				if depth == 1 {
					cur = map[string]string{}
				}
			case "key":
				var s string
				_ = dec.DecodeElement(&s, &t)
				key = s
			case "string", "integer", "real":
				var s string
				_ = dec.DecodeElement(&s, &t)
				if cur != nil && depth == 1 && key != "" {
					cur[key] = s
				}
				key = ""
			case "true", "false":
				if cur != nil && depth == 1 && key != "" {
					cur[key] = t.Name.Local
				}
				key = ""
			}
		case xml.EndElement:
			if t.Name.Local == "dict" {
				if depth == 1 && cur != nil {
					out = append(out, cur)
					cur = nil
				}
				depth--
			}
		}
	}
	return out
}
