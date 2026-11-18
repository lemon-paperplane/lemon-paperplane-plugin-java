package java

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// BuildCommandParams 是 buildCommand 服务的输入（spring-boot、kotlin 等插件经 host/call 调用）。
type BuildCommandParams struct {
	JDK         sdk.Toolchain     `json:"jdk"`
	Classpath   []string          `json:"classpath"`
	MainClass   string            `json:"mainClass"`
	VMOptions   string            `json:"vmOptions,omitempty"`   // 用户填写的 VM 参数，按 shell 规则切分
	ProgramArgs string            `json:"programArgs,omitempty"` // 用户填写的程序参数，按 shell 规则切分
	ExtraVMArgs []string          `json:"extraVmArgs,omitempty"` // 调用方附加的 VM 参数，排在用户参数之前（用户参数可覆盖）
	ExtraArgs   []string          `json:"extraArgs,omitempty"`   // 调用方附加的程序参数，排在用户参数之前
	Cwd         string            `json:"cwd"`
	Env         map[string]string `json:"env,omitempty"`
	Key         string            `json:"key,omitempty"` // argfile / classpath jar 的文件名，建议用运行配置 id
}

// buildCommand 生成 java 启动命令。JDK 9+ 把类路径写进 @argfile，JDK 8 生成只含 Class-Path 清单的 jar，避免命令行超长。
func buildCommand(cacheDir string, p *BuildCommandParams) (*sdk.LaunchSpec, error) {
	if p.JDK.Path == "" && p.JDK.Executable == "" {
		return nil, fmt.Errorf("jdk missing")
	}
	if p.MainClass == "" {
		return nil, fmt.Errorf("main class missing")
	}
	javaExe := p.JDK.Executable
	if javaExe == "" {
		javaExe = filepath.Join(p.JDK.Path, "bin", sdk.ExeName("java"))
	}
	major := JavaMajor(p.JDK.Version)
	userVM := sdk.SplitArgs(p.VMOptions)
	var vm []string
	vm = append(vm, p.ExtraVMArgs...)
	vm = append(vm, encodingArgs(major, append(append([]string{}, p.ExtraVMArgs...), userVM...))...)
	vm = append(vm, userVM...)

	cp := cleanClasspath(p.Classpath)
	key := p.Key
	if key == "" {
		key = sdk.StableID(append([]string{p.MainClass}, cp...)...)
	}
	key = safeFileName(key)
	var cpArgs []string
	display := ""
	if len(cp) > 0 {
		if major > 0 && major < 9 {
			jar := filepath.Join(cacheDir, "classpath", key+".jar")
			if err := writeClasspathJar(jar, cp); err != nil {
				return nil, err
			}
			cpArgs = []string{"-cp", jar}
			display = "-cp " + filepath.Base(jar)
		} else {
			file := filepath.Join(cacheDir, "argfiles", key+".argfile")
			if err := writeArgFile(file, []string{"-cp", strings.Join(cp, string(os.PathListSeparator))}); err != nil {
				return nil, err
			}
			cpArgs = []string{"@" + file}
			display = fmt.Sprintf("-cp <%d>", len(cp))
		}
	}
	args := append([]string{}, vm...)
	args = append(args, cpArgs...)
	args = append(args, p.MainClass)
	args = append(args, p.ExtraArgs...)
	args = append(args, sdk.SplitArgs(p.ProgramArgs)...)

	disp := []string{"java"}
	disp = append(disp, vm...)
	if display != "" {
		disp = append(disp, display)
	}
	disp = append(disp, p.MainClass)
	disp = append(disp, p.ExtraArgs...)
	disp = append(disp, sdk.SplitArgs(p.ProgramArgs)...)
	return &sdk.LaunchSpec{
		Command: javaExe,
		Args:    args,
		Cwd:     p.Cwd,
		Env:     p.Env,
		Display: strings.Join(disp, " "),
	}, nil
}

// encodingArgs 与 IDEA 一样默认按 UTF-8 输出，避免 Windows 上控制台乱码；用户已指定的不覆盖。
func encodingArgs(major int, existing []string) []string {
	has := func(prefix string) bool {
		for _, a := range existing {
			if strings.HasPrefix(a, prefix) {
				return true
			}
		}
		return false
	}
	var out []string
	if !has("-Dfile.encoding=") {
		out = append(out, "-Dfile.encoding=UTF-8")
	}
	if major >= 19 {
		if !has("-Dstdout.encoding=") {
			out = append(out, "-Dstdout.encoding=UTF-8")
		}
		if !has("-Dstderr.encoding=") {
			out = append(out, "-Dstderr.encoding=UTF-8")
		}
	} else if runtime.GOOS == "windows" {
		if !has("-Dsun.stdout.encoding=") {
			out = append(out, "-Dsun.stdout.encoding=UTF-8", "-Dsun.stderr.encoding=UTF-8")
		}
	}
	return out
}

// cleanClasspath 去空、去重，保持顺序。
func cleanClasspath(cp []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range cp {
		e = strings.TrimSpace(e)
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}

// quoteArgFileArg 按 java @argfile 规则给参数加引号：反斜杠与双引号需转义。
func quoteArgFileArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func writeArgFile(path string, args []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, a := range args {
		b.WriteString(quoteArgFileArg(a))
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// classpathURL 把路径转成清单 Class-Path 里的 URL；目录以 / 结尾。
func classpathURL(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	slash := filepath.ToSlash(abs)
	if !strings.HasPrefix(slash, "/") {
		slash = "/" + slash // Windows：C:/x → /C:/x
	}
	if st, err := os.Stat(p); err == nil && st.IsDir() && !strings.HasSuffix(slash, "/") {
		slash += "/"
	} else if err != nil && !strings.HasSuffix(strings.ToLower(slash), ".jar") && !strings.HasSuffix(strings.ToLower(slash), ".zip") && !strings.HasSuffix(slash, "/") {
		// 还不存在的条目（如尚未生成的资源目录）按目录处理
		slash += "/"
	}
	u := url.URL{Scheme: "file", Path: slash}
	return u.String()
}

// manifestLines 按清单格式写一个属性：每行最多 72 字节，续行以空格开头。
func manifestLines(name, value string) string {
	line := []byte(name + ": " + value)
	var b bytes.Buffer
	first := true
	for len(line) > 0 {
		max := 72
		if !first {
			max = 71
			b.WriteByte(' ')
		}
		n := max
		if n > len(line) {
			n = len(line)
		}
		// 不在多字节字符中间断开
		for n < len(line) && n > 0 && line[n]&0xC0 == 0x80 {
			n--
		}
		b.Write(line[:n])
		b.WriteString("\r\n")
		line = line[n:]
		first = false
	}
	return b.String()
}

// writeClasspathJar 生成只有清单的 jar（JDK 8 的「classpath jar」方案）。
func writeClasspathJar(path string, cp []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var urls []string
	for _, e := range cp {
		urls = append(urls, classpathURL(e))
	}
	manifest := "Manifest-Version: 1.0\r\n" + manifestLines("Class-Path", strings.Join(urls, " ")) + manifestLines("Created-By", "Lemon Paperplane") + "\r\n"
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("META-INF/MANIFEST.MF")
	if err == nil {
		_, err = w.Write([]byte(manifest))
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
