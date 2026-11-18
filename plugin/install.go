package java

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

const adoptiumAPI = "https://api.adoptium.net/v3"

// 拿不到 Adoptium API 时使用的 LTS 列表。
var fallbackLTS = []int{25, 21, 17, 11, 8}

type availableReleases struct {
	LTS          []int `json:"available_lts_releases"`
	Releases     []int `json:"available_releases"`
	MostRecent   int   `json:"most_recent_feature_release"`
	MostRecentLT int   `json:"most_recent_lts"`
}

type adoptiumAsset struct {
	Binary struct {
		Package struct {
			Name     string `json:"name"`
			Link     string `json:"link"`
			Checksum string `json:"checksum"`
			Size     int64  `json:"size"`
		} `json:"package"`
	} `json:"binary"`
	Version struct {
		Semver         string `json:"semver"`
		OpenJDKVersion string `json:"openjdk_version"`
		Major          int    `json:"major"`
	} `json:"version"`
	ReleaseName string `json:"release_name"`
}

func httpGet(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "LemonPaperplane")
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func fetchReleases(ctx context.Context) (*availableReleases, error) {
	b, err := httpGet(ctx, adoptiumAPI+"/info/available_releases", 15*time.Second)
	if err != nil {
		return nil, err
	}
	var r availableReleases
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// availableVersions 列出可下载的 Temurin 版本：所有 LTS + 比最新 LTS 更新的版本。
func (p *Plugin) availableVersions(ctx *sdk.Context) []sdk.AvailableVersion {
	rel, err := fetchReleases(ctx)
	var majors []int
	lts := map[int]bool{}
	rec := 21
	if err == nil && len(rel.Releases) > 0 {
		for _, v := range rel.LTS {
			lts[v] = true
		}
		for _, v := range rel.Releases {
			if v >= 8 && (lts[v] || v > rel.MostRecentLT) {
				majors = append(majors, v)
			}
		}
		rec = rel.MostRecentLT
	} else {
		for _, v := range fallbackLTS {
			lts[v] = true
		}
		majors = append(majors, fallbackLTS...)
		rec = fallbackLTS[0]
	}
	sort.Sort(sort.Reverse(sort.IntSlice(majors)))
	var out []sdk.AvailableVersion
	for _, m := range majors {
		label := ctx.T("toolchain.temurin", m)
		if lts[m] {
			label = ctx.T("toolchain.temurinLTS", m)
		}
		out = append(out, sdk.AvailableVersion{Version: strconv.Itoa(m), Label: label, Vendor: "Temurin", LTS: lts[m], Recommended: m == rec})
	}
	return out
}

// pickMajor 把安装请求（具体版本、主版本或约束）解析成 Temurin 主版本。
func pickMajor(req string, available []sdk.AvailableVersion) int {
	req = strings.TrimSpace(req)
	if m, err := strconv.Atoi(req); err == nil {
		return m
	}
	if regexp.MustCompile(`^\d+(\.\d+)*([._+-].*)?$`).MatchString(req) || strings.HasPrefix(req, "1.") {
		if m := JavaMajor(req); m > 0 {
			return m
		}
	}
	// 约束：优先满足约束的最高 LTS，其次最高版本
	c := sdk.ParseConstraint(req)
	best, bestLTS := 0, 0
	for _, a := range available {
		m, _ := strconv.Atoi(a.Version)
		v, _ := sdk.ParseVersion(a.Version + ".0.0")
		if !c.Check(v) {
			continue
		}
		if m > best {
			best = m
		}
		if a.LTS && m > bestLTS {
			bestLTS = m
		}
	}
	if bestLTS > 0 {
		return bestLTS
	}
	return best
}

func adoptiumOS(goos string) string {
	switch goos {
	case "darwin":
		return "mac"
	case "windows":
		return "windows"
	}
	return "linux"
}

func adoptiumArch(goarch string) string {
	switch goarch {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x64"
	case "386":
		return "x32"
	case "arm":
		return "arm"
	case "ppc64le":
		return "ppc64le"
	case "s390x":
		return "s390x"
	case "riscv64":
		return "riscv64"
	}
	return goarch
}

// adoptiumMirrorURL 拼出镜像地址：<镜像>/<major>/jdk/<arch>/<os>/<文件名>（清华等镜像的目录结构）。
func adoptiumMirrorURL(mirror string, major int, arch, os, file string) string {
	if mirror == "" {
		return ""
	}
	return fmt.Sprintf("%s/%d/jdk/%s/%s/%s", strings.TrimRight(mirror, "/"), major, arch, os, file)
}

var temurinFileRe = regexp.MustCompile(`href="(OpenJDK\d+U-jdk_[^"]+?\.(?:tar\.gz|zip))"`)

// temurinVersionFromFile 从文件名里取版本：OpenJDK17U-jdk_aarch64_mac_hotspot_17.0.21_9.tar.gz → 17.0.21；
// OpenJDK8U-jdk_x64_linux_hotspot_8u472b08.tar.gz → 8.0.472。
func temurinVersionFromFile(name string) string {
	if m := regexp.MustCompile(`hotspot_(\d+)u(\d+)b\d+`).FindStringSubmatch(name); m != nil {
		return m[1] + ".0." + m[2]
	}
	if m := regexp.MustCompile(`hotspot_(\d+(?:\.\d+)*)(?:_\d+)?\.`).FindStringSubmatch(name); m != nil {
		return NormalizeJavaVersion(m[1])
	}
	return ""
}

// installPlan 生成 Temurin 下载计划（镜像在前，GitHub 原链接在后）。
func (p *Plugin) installPlan(ctx *sdk.Context, req string) (*sdk.InstallPlan, error) {
	goos, goarch := "", ""
	if ctx.Init != nil {
		goos, goarch = ctx.Init.OS, ctx.Init.Arch
	}
	if goos == "" {
		goos, goarch = runtimeGOOS, runtimeGOARCH
	}
	osName, arch := adoptiumOS(goos), adoptiumArch(goarch)
	mirror := ""
	if ctx.Init != nil {
		mirror = ctx.Init.Mirrors.JdkAdoptium
	}
	major := pickMajor(req, p.availableVersions(ctx))
	if major <= 0 {
		return nil, sdk.UserError("%s", ctx.T("err.noMatchingJdk", req))
	}
	archive := "tar.gz"
	if osName == "windows" {
		archive = "zip"
	}
	url := fmt.Sprintf("%s/assets/latest/%d/hotspot?architecture=%s&image_type=jdk&os=%s&vendor=eclipse", adoptiumAPI, major, arch, osName)
	if b, err := httpGet(ctx, url, 20*time.Second); err == nil {
		var assets []adoptiumAsset
		if json.Unmarshal(b, &assets) == nil {
			for _, a := range assets {
				name := a.Binary.Package.Name
				if name == "" || !strings.HasSuffix(name, "."+archive) {
					continue
				}
				urls := []string{}
				if m := adoptiumMirrorURL(mirror, major, arch, osName, name); m != "" {
					urls = append(urls, m)
				}
				urls = append(urls, a.Binary.Package.Link)
				version := NormalizeJavaVersion(a.Version.Semver)
				if version == "" {
					version = temurinVersionFromFile(name)
				}
				return &sdk.InstallPlan{Version: version, URLs: urls, SHA256: a.Binary.Package.Checksum, Archive: archive, StripComponents: 1, FileName: name}, nil
			}
			if len(assets) == 0 {
				return nil, sdk.UserError("%s", ctx.T("err.temurinUnavailable", major, osName, arch))
			}
		}
	}
	// API 不通：直接列镜像目录挑最新的文件
	if mirror != "" {
		dir := fmt.Sprintf("%s/%d/jdk/%s/%s/", strings.TrimRight(mirror, "/"), major, arch, osName)
		if b, err := httpGet(ctx, dir, 20*time.Second); err == nil {
			var best, bestVer string
			for _, m := range temurinFileRe.FindAllStringSubmatch(string(b), -1) {
				name := m[1]
				if !strings.HasSuffix(name, "."+archive) || !strings.Contains(name, "_hotspot_") {
					continue
				}
				v := temurinVersionFromFile(name)
				if best == "" || sdk.CompareVersions(v, bestVer) > 0 {
					best, bestVer = name, v
				}
			}
			if best != "" {
				sha := ""
				if b, err := httpGet(ctx, dir+best+".sha256.txt", 10*time.Second); err == nil {
					if f := strings.Fields(string(b)); len(f) > 0 && len(f[0]) == 64 {
						sha = f[0]
					}
				}
				return &sdk.InstallPlan{Version: bestVer, URLs: []string{dir + best}, SHA256: sha, Archive: archive, StripComponents: 1, FileName: best}, nil
			}
		}
	}
	return nil, sdk.UserError("%s", ctx.T("err.jdkIndexUnavailable"))
}
