package java

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func TestNormalizeJavaVersion(t *testing.T) {
	cases := map[string]string{
		"1.8.0_492":  "8.0.492",
		"1.8.0":      "8.0.0",
		"1.7.0_80":   "7.0.80",
		"17":         "17.0.0",
		"17.0.16":    "17.0.16",
		"21.0.12.1":  "21.0.12.1",
		"17.0.2+8":   "17.0.2",
		`"26.0.1"`:   "26.0.1",
		"22-ea":      "22.0.0-ea",
		"11.0.2_9":   "11.0.2.9",
		"":           "",
		"1.8.0_472b": "8.0.0",
	}
	for in, want := range cases {
		if got := NormalizeJavaVersion(in); got != want {
			t.Errorf("NormalizeJavaVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if JavaMajor("1.8") != 8 || JavaMajor("17.0.1") != 17 || JavaMajor("x") != 0 {
		t.Error("JavaMajor")
	}
}

func TestParseRelease(t *testing.T) {
	for file, want := range map[string][3]string{
		"release-zulu8":      {"8.0.492", "Zulu", "arm64"},
		"release-corretto17": {"17.0.16", "Corretto", "arm64"},
	} {
		f, err := os.Open(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		rel := parseRelease(f)
		f.Close()
		got := [3]string{NormalizeJavaVersion(rel["JAVA_VERSION"]), vendorName(rel), normArch(rel["OS_ARCH"])}
		if got != want {
			t.Errorf("%s: %v, want %v", file, got, want)
		}
	}
	if vendorName(map[string]string{"IMPLEMENTOR": "Eclipse Adoptium"}) != "Temurin" {
		t.Error("Temurin")
	}
	if vendorName(map[string]string{"IMPLEMENTOR": "Homebrew"}) != "Homebrew" {
		t.Error("Homebrew")
	}
}

func TestParseJavaHomePlist(t *testing.T) {
	b, _ := os.ReadFile("testdata/java_home.plist")
	d := parseJavaHomePlist(b)
	if len(d) != 2 {
		t.Fatalf("got %d dicts", len(d))
	}
	if d[0]["JVMHomePath"] != "/Users/x/Library/Java/JavaVirtualMachines/ms-21.0.12.1/Contents/Home" || d[0]["JVMEnabled"] != "true" {
		t.Errorf("dict0 %v", d[0])
	}
	if d[1]["JVMVersion"] != "1.8.0_492" {
		t.Errorf("dict1 %v", d[1])
	}
}

// 用假 JDK 目录测发现：Contents/Home 布局、JRE 排除、符号链接去重、Paths 只查给定目录。
func TestDiscoverPaths(t *testing.T) {
	root := t.TempDir()
	mk := func(home, version string, javac bool) {
		_ = os.MkdirAll(filepath.Join(home, "bin"), 0o755)
		_ = os.WriteFile(filepath.Join(home, "bin", sdk.ExeName("java")), nil, 0o755)
		if javac {
			_ = os.WriteFile(filepath.Join(home, "bin", sdk.ExeName("javac")), nil, 0o755)
		}
		_ = os.WriteFile(filepath.Join(home, "release"), []byte("IMPLEMENTOR=\"Eclipse Adoptium\"\nJAVA_VERSION=\""+version+"\"\nOS_ARCH=\"x86_64\"\n"), 0o644)
	}
	mk(filepath.Join(root, "jdk-17", "Contents", "Home"), "17.0.21", true)
	mk(filepath.Join(root, "jre-8"), "1.8.0_400", false)
	mk(filepath.Join(root, "jdk-21"), "21.0.9", true)
	if runtime.GOOS != "windows" {
		_ = os.Symlink(filepath.Join(root, "jdk-21"), filepath.Join(root, "link-21"))
	}
	init := &sdk.InitializeParams{HostDataDir: root}
	tcs := discoverJDKs(init, []string{filepath.Join(root, "jdk-17"), filepath.Join(root, "jre-8"), filepath.Join(root, "jdk-21"), filepath.Join(root, "link-21")})
	if len(tcs) != 2 {
		t.Fatalf("got %d toolchains: %+v", len(tcs), tcs)
	}
	if tcs[0].Version != "21.0.9" || tcs[1].Version != "17.0.21" {
		t.Errorf("order/version: %s %s", tcs[0].Version, tcs[1].Version)
	}
	jdk17 := tcs[1]
	if !strings.HasSuffix(jdk17.Path, filepath.Join("Contents", "Home")) || jdk17.Env["JAVA_HOME"] != jdk17.Path || jdk17.Vendor != "Temurin" || jdk17.Arch != "amd64" {
		t.Errorf("jdk17 %+v", jdk17)
	}
	if len(jdk17.PathPrepend) != 1 || jdk17.Label != "Temurin 17.0.21" {
		t.Errorf("jdk17 label/path %+v", jdk17)
	}
}

func TestMainClassDetection(t *testing.T) {
	cases := []struct {
		src, want string
	}{
		{"package a.b;\npublic class X { public static void main(String[] args) {} }", "a.b.X"},
		{"class X { static public void main(String args[]) {} }", "X"},
		{"package a; class X { public static void main(final java.lang.String... a) {} }", "a.X"},
		{"package a; class X { void main() {} }", ""},
		{"package a; @SpringBootApplication class X { public static void main(String[] a) {} }", ""},
	}
	for _, c := range cases {
		if got := mainClassFromSource([]byte(c.src), "X"); got != c.want {
			t.Errorf("%q → %q, want %q", c.src, got, c.want)
		}
	}
	projects := []subproject{{Path: ":app", Dir: "app"}, {Path: ":lib", Dir: "lib"}}
	got := scanMainClasses("testdata/gradle-multi", "gradle", projects)
	if len(got) != 1 || got[0].Class != "com.acme.Tool" || got[0].Project != ":app" {
		t.Errorf("gradle-multi: %+v", got)
	}
	plain := scanMainClasses("testdata/plain", "none", []subproject{{}})
	if len(plain) != 1 || plain[0].Class != "com.acme.Main" {
		t.Errorf("plain: %+v", plain)
	}
}

func TestDetect(t *testing.T) {
	p := &Plugin{}
	ctx := &sdk.Context{}
	r, _ := p.Detect(ctx, &sdk.DetectParams{Dir: "testdata/gradle-multi", Files: []string{"settings.gradle", "app/", "lib/"}})
	if len(r.Facets) != 1 || r.Facets[0].Data["buildTool"] != "gradle" {
		t.Errorf("gradle-multi: %+v", r)
	}
	r, _ = p.Detect(ctx, &sdk.DetectParams{Dir: "testdata/plain", Files: []string{"src/"}})
	if len(r.Facets) != 1 || r.Facets[0].Data["buildTool"] != "none" {
		t.Errorf("plain: %+v", r)
	}
	r, _ = p.Detect(ctx, &sdk.DetectParams{Dir: "testdata/plain/src/com", RelDir: "x/src/com", Files: []string{"A.java"}})
	if len(r.Facets) != 0 {
		t.Errorf("source tree should not be a module: %+v", r)
	}
}

func TestIdeaModuleMapping(t *testing.T) {
	cases := []struct{ in, root, tool, want string }{
		{"vrtalk-api.vrtalk-api-starter.main", "vrtalk-api", "gradle", ":vrtalk-api-starter"},
		{"vrtalk-api.main", "vrtalk-api", "gradle", ":"},
		{"root.a.b.test", "root", "gradle", ":a:b"},
		{"demo-app", "parent", "maven", "demo-app"},
		{"", "x", "gradle", ""},
	}
	for _, c := range cases {
		if got := ideaModuleToProject(c.in, c.root, c.tool); got != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildCommandArgFile(t *testing.T) {
	dir := t.TempDir()
	spec, err := buildCommand(dir, &BuildCommandParams{
		JDK:         sdk.Toolchain{Path: "/jdk", Version: "17.0.16"},
		Classpath:   []string{"/a/classes", `/b/my lib\x.jar`, "/a/classes", ""},
		MainClass:   "com.acme.Main",
		VMOptions:   `-Xmx1g -Dname="a b"`,
		ProgramArgs: "--port 9000",
		ExtraVMArgs: []string{"-XX:TieredStopAtLevel=1"},
		Cwd:         "/work",
		Key:         "cfg/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	enc := []string{"-Dfile.encoding=UTF-8"}
	if runtime.GOOS == "windows" {
		// Windows 控制台默认 GBK 等本地编码，输出接到管道时也要强制 UTF-8
		enc = append(enc, "-Dsun.stdout.encoding=UTF-8", "-Dsun.stderr.encoding=UTF-8")
	}
	want := append(append([]string{"-XX:TieredStopAtLevel=1"}, enc...), "-Xmx1g", "-Dname=a b", "@"+filepath.Join(dir, "argfiles", "cfg_1.argfile"), "com.acme.Main", "--port", "9000")
	if strings.Join(spec.Args, "|") != strings.Join(want, "|") {
		t.Errorf("args %q", spec.Args)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "argfiles", "cfg_1.argfile"))
	sep := string(os.PathListSeparator)
	if string(b) != "\"-cp\"\n\"/a/classes"+sep+`/b/my lib\\x.jar`+"\"\n" {
		t.Errorf("argfile %q", b)
	}
	if spec.Command != filepath.Join("/jdk", "bin", sdk.ExeName("java")) || spec.Cwd != "/work" {
		t.Errorf("spec %+v", spec)
	}
}

func TestBuildCommandClasspathJar(t *testing.T) {
	dir := t.TempDir()
	classes := filepath.Join(dir, "classes")
	_ = os.MkdirAll(classes, 0o755)
	var cp []string
	cp = append(cp, classes)
	for i := 0; i < 30; i++ {
		cp = append(cp, filepath.Join(dir, "repo", "some-long-library-name-for-wrapping", "lib"+itoa(i)+".jar"))
	}
	spec, err := buildCommand(dir, &BuildCommandParams{JDK: sdk.Toolchain{Path: "/jdk8", Version: "8.0.492"}, Classpath: cp, MainClass: "Main", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(dir, "classpath", "k.jar")
	if spec.Args[len(spec.Args)-3] != "-cp" || spec.Args[len(spec.Args)-2] != jar {
		t.Fatalf("args %q", spec.Args)
	}
	zr, err := zip.OpenReader(jar)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	f, _ := zr.File[0].Open()
	data, _ := io.ReadAll(f)
	for _, line := range strings.Split(string(data), "\r\n") {
		if len(line) > 72 {
			t.Errorf("manifest line too long: %d", len(line))
		}
	}
	joined := strings.ReplaceAll(string(data), "\r\n ", "")
	if !strings.Contains(joined, classpathURL(classes)) || !strings.HasSuffix(classpathURL(classes), "/") {
		t.Errorf("manifest %q", joined)
	}
	if !strings.Contains(joined, "lib29.jar") {
		t.Error("missing last entry")
	}
}

func TestIncreaseOption(t *testing.T) {
	v, size := increaseOption("-Dx=1 -Xmx1g", xmxRe, "-Xmx", 2048)
	if v != "-Dx=1 -Xmx2g" || size != "2g" {
		t.Errorf("%q %q", v, size)
	}
	v, _ = increaseOption("-Xmx4096m -Dy", xmxRe, "-Xmx", 2048)
	if v != "-Dy -Xmx8g" {
		t.Errorf("%q", v)
	}
	v, _ = increaseOption("", metaspaceRe, "-XX:MaxMetaspaceSize=", 512)
	if v != "-XX:MaxMetaspaceSize=512m" {
		t.Errorf("%q", v)
	}
}

func TestPickMajorAndTemurinFile(t *testing.T) {
	av := []sdk.AvailableVersion{{Version: "25", LTS: true}, {Version: "24"}, {Version: "21", LTS: true}, {Version: "17", LTS: true}, {Version: "11", LTS: true}, {Version: "8", LTS: true}}
	cases := map[string]int{"17": 17, "17.0.12": 17, "1.8": 8, ">=17 <24": 21, ">=24": 25, "<=11": 11, ">=11 <=17": 17}
	for in, want := range cases {
		if got := pickMajor(in, av); got != want {
			t.Errorf("pickMajor(%q)=%d want %d", in, got, want)
		}
	}
	if v := temurinVersionFromFile("OpenJDK17U-jdk_aarch64_mac_hotspot_17.0.21_9.tar.gz"); v != "17.0.21" {
		t.Error(v)
	}
	if v := temurinVersionFromFile("OpenJDK8U-jdk_x64_linux_hotspot_8u472b08.tar.gz"); v != "8.0.472" {
		t.Error(v)
	}
	if u := adoptiumMirrorURL("https://mirrors.tuna.tsinghua.edu.cn/Adoptium/", 17, "aarch64", "mac", "f.tar.gz"); u != "https://mirrors.tuna.tsinghua.edu.cn/Adoptium/17/jdk/aarch64/mac/f.tar.gz" {
		t.Error(u)
	}
}
