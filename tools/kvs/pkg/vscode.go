package pkg

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ------------------------------------------------------------------------------
// Custom VSCode feature
// handling the NLS garbled text issue from https://github.com/microsoft/vscode/issues/299425

// Processing logic:
// /stable-{commit}/web-extension-resource/{publisher}.vscode-unpkg.net/{publisher}/{name}/{version}/{path}
// prePath = /stable-{commit}/web-extension-resource/
// file = {svcHome}/extensions/{publisher}.{name}-{version}/{path}
// svc 扩展（svc://<name>.<ext>）：
//   - vscode.ext — /stable-<verhash>/web-extension-resource/ 的扩展资源
//   - vscode.nls — NLS 翻译重映射（挂载点来自路由，如 /__cache/vscode/nls/）
func init() {
	RegisterSvcExt("vscode", "ext", func(w http.ResponseWriter, r *http.Request, sc *ServiceConfig, mount string) {
		if sc.VersionHash == "" {
			http.NotFound(w, r)
			return
		}
		prePath := "/stable-" + sc.VersionHash + "/web-extension-resource/"
		if !strings.HasPrefix(r.URL.Path, prePath) {
			http.NotFound(w, r)
			return
		}
		VscodeExtHandle(w, r, prePath, sc.Home)
	})
	RegisterSvcExt("vscode", "nls", func(w http.ResponseWriter, r *http.Request, sc *ServiceConfig, mount string) {
		VscodeNlsHandle(w, r, mount, sc.Home, sc.BinHome, sc.VscLanguage)
	})
}

// VscodeExtHandle serves VS Code web-extension-resource requests.
func VscodeExtHandle(w http.ResponseWriter, r *http.Request, prePath, svcHome string) {
	// Strip prePath to get the relative path, e.g.:
	//   {publisher}.vscode-unpkg.net/{publisher}/{name}/{version}/{path}
	rel := strings.TrimPrefix(r.URL.Path, prePath)
	if rel == "" {
		http.NotFound(w, r)
		return
	}

	// Map URL path → local file path.
	// URL:   {host}/{publisher}/{name}/{version}/{path...}
	//        {host} = {publisher}.vscode-unpkg.net
	// Local: {svcHome}/extensions/{publisher}.{name}-{version}/{path}
	//
	// Split into 6 parts: [host, publisher, name, version, "extension", path]
	seg := strings.SplitN(rel, "/", 6)
	if len(seg) < 6 {
		http.NotFound(w, r)
		return
	}
	publisher := seg[1] // {publisher}
	name := seg[2]      // {name}
	version := seg[3]   // {version}
	rest := seg[5]      // {path...}

	// Local file path: {svcHome}/{publisher}.{name}-{version}/{path}
	filePath := filepath.Join(svcHome, "extensions", strings.ToLower(publisher+"."+name+"-"+version), rest)

	// Security: ensure the resolved path stays within svcHome (path traversal guard).
	if !isPathWithin(filePath, svcHome) {
		log.Printf("[ext] WARNING: path traversal attempt: %s", rel)
		http.NotFound(w, r)
		return
	}

	// Serve the file directly from disk.
	// log.Printf("[ext] serving: %s", filePath)
	http.ServeFile(w, r, filePath)
}

// A. https://marketplace.visualstudio.com/_apis/public/gallery/vscode/ms-ceintl/vscode-language-pack-zh-hans/latest
// B. https://marketplace.visualstudio.com/_apis/public/gallery/publishers/ms-ceintl/vsextensions/vscode-language-pack-zh-hans/latest/vspackage
// C. https://MS-CEINTL.vscode-unpkg.net/MS-CEINTL/vscode-language-pack-zh-hans/1.131.2026072717/extension/translations/main.i18n.json
// D. https://marketplace.visualstudio.com/_apis/public/gallery/publishers/ms-ceintl/vsextensions/vscode-language-pack-zh-hans/1.131.2026072717/vspackage
// E. https://example.com/__cache/vscode/nls/a5b500951314efd502d07465bd138dfbd714a960/1.133.0/zh-cn/nls-messages.js
// F. {svcHome}/cache/ccproxy/__cache/vscode/nls/a5b500951314efd502d07465bd138dfbd714a960/1.133.0/zh-cn/
// G. {svcHome}/extensions/ms-ceintl.vscode-language-pack-zh-hans-1.131.2026072717/translations/
// H. {binHome}/out/nls.keys.json + {binHome}/out/nls.messages.json
//    # the local app's nls.keys.json order differs from the CDN's; nls.messages.json is the English fallback.

// Processing logic:
// 1. 通过 E 获取当前 commit, version, lang:
//    commit = a5b500951314efd502d07465bd138dfbd714a960, version = 1.133.0, lang = zh-cn
// 2. 检查本地缓存文件 F/nls.messages.js 是否存在， 如果存在，直接返回
// 3. 命中 → 直接返回 nls.messages.js
// 4. 未命中 → 下载 A, 通过 A 中的 versions[0].version 获取版本号
//        判断 G/main.i18n.json 是否存在， 如果存在，跳到 6
//        判断 F/ms-ceintl.vscode-language-pack-zh-hans-1.131.2026072717.vsix 是否存在, 如果存在，跳过
//        如果不存在通过 D 下载 F/ms-ceintl.vscode-language-pack-zh-hans-1.131.2026072717.vsix
// 5. 通过 {binHome}/bin/remote-cli/code 安装 (及时重复安装了， 也没有关系)
//        安装完成后，确认 G/main.i18n.json 存在, 如果不存在，证明安装失败了, 报错
// 6. H + G/main.i18n.json -> F/nls.messages.json -> F/nls.messages.js, 并进行 gzip 压缩并缓存， nls.messages.json（保留， 不缩进）
//    遍历 nls.keys.json, 对每个 (module, key) 在 G/main.i18n.json 的 contents 中查找翻译，
//    找不到则回退到 H/nls.messages.json 中的英文消息。

// VscodeNlsHandle intercepts VS Code NLS requests for translation remapping.
func VscodeNlsHandle(w http.ResponseWriter, r *http.Request, prePath, svcHome, binHome string, langMap map[string]string) {
	cacheOnce.Do(initCache)

	// 1. Parse URL: {prePath}{commit}/{version}/{lang}/nls-messages.js
	//    prePath is KVS_SVC_VSCODE_NLS_URL, e.g. "/__cache/vscode/nls/"
	//    After stripping prePath: "{commit}/{version}/{lang}/nls-messages.js"
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prePath), "/")
	if len(parts) < 4 || (parts[3] != "nls-messages.js" && parts[3] != "nls.messages.js") {
		http.Error(w, "invalid NLS path, expected "+prePath+"{commit}/{version}/{lang}/nls-messages.js", http.StatusBadRequest)
		return
	}
	commit := parts[0]
	version := parts[1]
	lang := parts[2]

	// Resolve lang → langpack name via vsc_language map (e.g. zh-cn→zh-hans).
	// Languages absent from the map default to lang itself (symmetric).
	langPack := lang
	if lp, ok := langMap[lang]; ok && lp != "" {
		langPack = lp
	}

	// 2. Cache paths: {cacheRoot}/vscode/nls/{commit}/{version}/{lang}/
	//    nls.messages.js       → gzip-compressed processed JS body
	//    nls.messages.js_.json → cache metadata (headers)
	//    nls.messages.json     → raw merged message array (kept, not indented)
	nlsCacheDir := filepath.Join(cacheRoot, "vscode", "nls", commit, version, lang)
	bodyPath := filepath.Join(nlsCacheDir, "nls.messages.js")
	metaPath := bodyPath + "_.json"
	jsonPath := filepath.Join(nlsCacheDir, "nls.messages.json")

	// 3. Cache HIT → return cached gzip body with stored headers.
	if meta, err := readCacheMeta(metaPath); err == nil {
		if body, err := os.ReadFile(bodyPath); err == nil {
			w.Header().Set("X-Cache", "HIT")
			for k, vs := range meta.Headers {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(meta.Status)
			_, _ = w.Write(body)
			return
		}
	}

	// 4. Cache MISS → download A (marketplace latest JSON) to get version.
	//    A: https://marketplace.visualstudio.com/_apis/public/gallery/vscode/ms-ceintl/vscode-language-pack-{langPack}/latest
	const mpBase = "https://marketplace.visualstudio.com/_apis/public/gallery"
	latestURL := mpBase + "/vscode/ms-ceintl/vscode-language-pack-" + langPack + "/latest"
	latestData, err := vscodeNlsFetchBytes(latestURL)
	if err != nil {
		http.Error(w, "failed to fetch language-pack latest: "+err.Error(), http.StatusBadGateway)
		return
	}
	var latestMeta map[string]any
	if err := json.Unmarshal(latestData, &latestMeta); err != nil {
		http.Error(w, "failed to parse language-pack latest JSON: "+err.Error(), http.StatusBadGateway)
		return
	}
	// versions is an array; the first element is the latest version, e.g. "1.131.2026072717".
	packVersion := ""
	if versions, ok := latestMeta["versions"].([]any); ok && len(versions) > 0 {
		if v0, ok := versions[0].(map[string]any); ok {
			if v, ok := v0["version"].(string); ok {
				packVersion = v
			}
		}
	}
	if packVersion == "" {
		http.Error(w, "missing versions[0].version in language-pack latest JSON", http.StatusBadGateway)
		return
	}
	log.Printf("[nls] language-pack %s version: %s", langPack, packVersion)

	// G: {svcHome}/extensions/ms-ceintl.vscode-language-pack-{langPack}-{packVersion}/translations/
	extDir := filepath.Join(svcHome, "extensions", "ms-ceintl.vscode-language-pack-"+langPack+"-"+packVersion)
	i18nPath := filepath.Join(extDir, "translations", "main.i18n.json")

	// If G/main.i18n.json already exists, skip download + install entirely
	// (the extension was installed in a previous run; no need to re-download
	// the vsix or re-install, which avoids "Please restart VS Code" errors).
	if _, err := os.Stat(i18nPath); err == nil {
		log.Printf("[nls] translations already exist, skipping download+install: %s", i18nPath)
	} else {
		// Determine vsix cache path: F/ms-ceintl.vscode-language-pack-{langPack}-{packVersion}.vsix
		vsixName := "ms-ceintl.vscode-language-pack-" + langPack + "-" + packVersion + ".vsix"
		vsixPath := filepath.Join(nlsCacheDir, vsixName)

		// Download vsix via D if not cached.
		// D: {mpBase}/publishers/ms-ceintl/vsextensions/vscode-language-pack-{langPack}/{packVersion}/vspackage
		if _, err := os.Stat(vsixPath); err != nil {
			downloadURL := mpBase + "/publishers/ms-ceintl/vsextensions/vscode-language-pack-" + langPack + "/" + packVersion + "/vspackage"
			log.Printf("[nls] downloading vsix: %s → %s", downloadURL, vsixPath)
			dlClient := buildDownloadClient("")
			if err := os.MkdirAll(nlsCacheDir, 0o755); err != nil {
				http.Error(w, "mkdir cache dir failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if err := downloadFile(dlClient, downloadURL, vsixPath, nil); err != nil {
				http.Error(w, "failed to download vsix: "+err.Error(), http.StatusBadGateway)
				return
			}
			log.Printf("[nls] vsix downloaded: %s", vsixPath)
		} else {
			log.Printf("[nls] vsix cached: %s", vsixPath)
		}

		// 5. Install language-pack extension via code-server.
		codeServer := filepath.Join(binHome, "bin", "code-server")
		installCmd := codeServer + " --install-extension " + vsixPath + " --server-data-dir " + svcHome + " --accept-server-license-terms"
		log.Printf("[nls] installing extension: %s", installCmd)
		if err := RunServiceStartup(installCmd); err != nil {
			http.Error(w, "failed to install language-pack: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Confirm G/main.i18n.json exists after installation.
		if _, err := os.Stat(i18nPath); err != nil {
			http.Error(w, "language-pack installed but main.i18n.json not found at "+i18nPath+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("[nls] translations found: %s", i18nPath)
	}

	// 6. Merge H (local nls.keys.json + nls.messages.json) + G/main.i18n.json
	//    → F/nls.messages.json (raw, not indented) → F/nls.messages.js (gzip)
	//    H: {binHome}/out/nls.keys.json, {binHome}/out/nls.messages.json
	keysData, err := os.ReadFile(filepath.Join(binHome, "out", "nls.keys.json"))
	if err != nil {
		http.Error(w, "failed to read local nls.keys.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var nlsKeys [][2]json.RawMessage
	if err := json.Unmarshal(keysData, &nlsKeys); err != nil {
		http.Error(w, "failed to parse local nls.keys.json: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// English fallback messages (flat array, same order as nls.keys.json).
	enMsgsData, err := os.ReadFile(filepath.Join(binHome, "out", "nls.messages.json"))
	if err != nil {
		http.Error(w, "failed to read local nls.messages.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var enMessages []string
	if err := json.Unmarshal(enMsgsData, &enMessages); err != nil {
		http.Error(w, "failed to parse local nls.messages.json: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Load translations: main.i18n.json → { "contents": { moduleId: { key: translated } } }
	i18nData, err := os.ReadFile(i18nPath)
	if err != nil {
		http.Error(w, "failed to read main.i18n.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var i18n struct {
		Contents map[string]map[string]string `json:"contents"`
	}
	if err := json.Unmarshal(i18nData, &i18n); err != nil {
		http.Error(w, "failed to parse main.i18n.json: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Merge: iterate nls.keys.json in order, look up each key in the
	// language pack; fall back to the English message from nls.messages.json.
	result := make([]string, 0, len(enMessages))
	idx := 0
	for _, entry := range nlsKeys {
		var module string
		if err := json.Unmarshal(entry[0], &module); err != nil {
			continue
		}
		var keys []string
		if err := json.Unmarshal(entry[1], &keys); err != nil {
			continue
		}
		moduleTranslations := i18n.Contents[module]
		for _, k := range keys {
			var msg string
			if moduleTranslations != nil {
				if t, ok := moduleTranslations[k]; ok && t != "" {
					msg = t
				} else if idx < len(enMessages) {
					msg = enMessages[idx]
				}
			} else if idx < len(enMessages) {
				msg = enMessages[idx]
			}
			result = append(result, msg)
			idx++
		}
	}

	// Write F/nls.messages.json (raw, not indented — compact JSON).
	compactJSON, err := json.Marshal(result)
	if err != nil {
		http.Error(w, "failed to marshal merged messages: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(nlsCacheDir, 0o755); err == nil {
		_ = atomicWriteFile(jsonPath, compactJSON, 0o644)
	}
	// Generate F/nls.messages.js (gzip-compressed JS).
	jsContent := []byte("/*---------------------------------------------------------\n" +
		" * Copyright (C) Microsoft Corporation. All rights reserved.\n" +
		" *--------------------------------------------------------*/\n" +
		"globalThis._VSCODE_NLS_MESSAGES=" + string(compactJSON) + ";\n" +
		"globalThis._VSCODE_NLS_LANGUAGE=" + strconv.Quote(lang) + ";\n")

	// Gzip compress the JS content for caching and response.
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	if _, err := gzw.Write(jsContent); err != nil {
		http.Error(w, "gzip compression failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := gzw.Close(); err != nil {
		http.Error(w, "gzip close failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	gzBody := gzBuf.Bytes()

	// Write to cache (atomic): nls.messages.js + metadata.
	if err := os.MkdirAll(nlsCacheDir, 0o755); err == nil {
		_ = atomicWriteFile(bodyPath, gzBody, 0o644)
		meta := &cacheMeta{
			Status: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type":     {"application/javascript; charset=utf-8"},
				"Content-Encoding": {"gzip"},
				"Cache-Control":    {"public, max-age=86400"},
				"Vary":             {"Accept-Encoding"},
			},
		}
		_ = writeCacheMeta(metaPath, meta)
	}

	// Return the gzip response to the client.
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(gzBody)
}

// vscodeNlsFetchBytes downloads a URL and returns the response body.
func vscodeNlsFetchBytes(urlStr string) ([]byte, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", urlStr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", urlStr, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", urlStr, err)
	}
	return body, nil
}

// ------------------------------------------------------------------------------
// Custom VSCode feature
// sync application image mirror to custom s3 server

// mirrorConfig holds the mirror command settings. The [mirror] config
// section no longer exists: everything comes from the command line and the
// KVS_MIRROR_S3_* environment variables.
type mirrorConfig struct {
	S3Prefix string // KVS_MIRROR_S3_PREFIX — e.g. https://oss.example.com/vsc
	S3Access string // KVS_MIRROR_S3_ACCESS — access key ID
	S3Secret string // KVS_MIRROR_S3_SECRET — secret access key
	S3Region string // KVS_MIRROR_S3_REGION — region (default empty)
	Platform string // -platform, e.g. server-linux-x64-web (default)
	BaseURL  string // vscode update API base (default: https://update.code.visualstudio.com)
}

// loadMirrorConfig reads the mirror settings from the environment
// (KVS_MIRROR_S3_PREFIX/_ACCESS/_SECRET/_REGION).
func loadMirrorConfig() mirrorConfig {
	return mirrorConfig{
		S3Prefix: os.Getenv("KVS_MIRROR_S3_PREFIX"),
		S3Access: os.Getenv("KVS_MIRROR_S3_ACCESS"),
		S3Secret: os.Getenv("KVS_MIRROR_S3_SECRET"),
		S3Region: os.Getenv("KVS_MIRROR_S3_REGION"),
		Platform: "server-linux-x64-web",
		BaseURL:  "https://update.code.visualstudio.com",
	}
}

// MirrorCommand syncs vscode versions to S3-compatible storage.
//
//	kvs mirror [vscode] [-version <v|latest>] [-platform <p>]
//
// The optional positional argument names the app to sync (only "vscode" is
// supported); -version selects the version ("latest", the default, first
// fetches /api/latest/ for the version name); -platform overrides the default
// server-linux-x64-web. Both flags accept "-x value" and "-x=value". Storage
// settings come from the KVS_MIRROR_S3_* environment variables; the metadata
// (url, hash, …) always comes from /api/versions/{name}/.
//
// S3 layout:
//   - {s3_prefix}/stable/{hash}/vscode-server-linux-x64-web.tar.gz   → tarball
//   - {s3_prefix}/api/latest/server-linux-x64-web/stable            → latest JSON (latest only)
//   - {s3_prefix}/api/versions/{name}/server-linux-x64-web/stable   → version JSON
func MirrorCommand(args []string) {
	// Parse arguments: one optional positional app name + flags.
	app, version, platform := "vscode", "latest", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-version" || a == "-platform":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "mirror: %s requires a value\nusage: kvs mirror [vscode] [-version <v|latest>] [-platform <p>]\n", a)
				os.Exit(1)
			}
			i++
			if a == "-version" {
				version = args[i]
			} else {
				platform = args[i]
			}
		case strings.HasPrefix(a, "-version="):
			version = strings.TrimPrefix(a, "-version=")
		case strings.HasPrefix(a, "-platform="):
			platform = strings.TrimPrefix(a, "-platform=")
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "mirror: unknown flag %q\nusage: kvs mirror [vscode] [-version <v|latest>] [-platform <p>]\n", a)
			os.Exit(1)
		default:
			app = a
		}
	}
	if app != "vscode" {
		fmt.Fprintf(os.Stderr, "mirror: unknown app %q (only vscode)\n", app)
		os.Exit(1)
	}
	if version == "" {
		log.Fatalf("[mirror] -version requires a value (e.g. -version 1.139.1 or -version latest)")
	}

	mc := loadMirrorConfig()
	if platform != "" {
		mc.Platform = platform
	}
	if mc.S3Prefix == "" {
		fmt.Fprintf(os.Stderr, "mirror: KVS_MIRROR_S3_PREFIX is not set, mirror disabled\n")
		os.Exit(1)
	}
	log.Printf("[mirror] s3_prefix=%s", mc.S3Prefix)

	client := buildDownloadClient("")

	// Step 1: Fetch version metadata JSON.
	// latest mode: first fetch /api/latest/ to get the version name, then
	// fetch /api/versions/{name}/ to get the full metadata (url, hash, etc.)
	// specified version: directly fetch /api/versions/{version}/
	var apiURL string
	if version == "latest" {
		latestAPI := mc.BaseURL + "/api/latest/" + mc.Platform + "/stable"
		log.Printf("[mirror] fetching latest: %s", latestAPI)
		resp, err := client.Get(latestAPI)
		if err != nil {
			log.Fatalf("[mirror] fetch latest: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			log.Fatalf("[mirror] latest API returned HTTP %d", resp.StatusCode)
		}
		latestJSON, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Fatalf("[mirror] read latest: %v", err)
		}
		log.Printf("[mirror] latest response: %s", string(latestJSON))
		var latestMeta map[string]any
		if err := json.Unmarshal(latestJSON, &latestMeta); err != nil {
			log.Fatalf("[mirror] parse latest JSON: %v", err)
		}
		latestName, _ := latestMeta["name"].(string)
		if latestName == "" {
			log.Fatalf("[mirror] missing name in latest response")
		}
		log.Printf("[mirror] latest version name: %s", latestName)
		apiURL = mc.BaseURL + "/api/versions/" + latestName + "/" + mc.Platform + "/stable"
	} else {
		apiURL = mc.BaseURL + "/api/versions/" + version + "/" + mc.Platform + "/stable"
	}
	log.Printf("[mirror] fetching %s", apiURL)
	resp, err := client.Get(apiURL)
	if err != nil {
		log.Fatalf("[mirror] fetch metadata: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		log.Fatalf("[mirror] metadata API returned HTTP %d", resp.StatusCode)
	}
	metaJSON, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		log.Fatalf("[mirror] read metadata: %v", err)
	}
	log.Printf("[mirror] metadata: %s", string(metaJSON))

	// Parse metadata JSON.
	var meta map[string]any
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		log.Fatalf("[mirror] parse metadata JSON: %v", err)
	}
	downloadURL, _ := meta["url"].(string)
	metaVersion, _ := meta["version"].(string) // commit hash
	metaName, _ := meta["name"].(string)       // version name e.g. 1.132.1
	if downloadURL == "" || metaVersion == "" {
		log.Fatalf("[mirror] missing url or version in metadata")
	}
	log.Printf("[mirror] version=%s hash=%s url=%s", metaName, metaVersion, downloadURL)

	// Step 2: Download the tarball to a temp file.
	tmpFile, err := os.CreateTemp("", "vscode-mirror-*.tar.gz")
	if err != nil {
		log.Fatalf("[mirror] create temp file: %v", err)
	}
	tarballPath := tmpFile.Name()
	defer os.Remove(tarballPath)
	log.Printf("[mirror] downloading tarball: %s", downloadURL)
	var lastDlPct int64 = -1
	if err := downloadFile(client, downloadURL, tarballPath, func(written, total int64) {
		if total > 0 {
			pct := written * 100 / total
			if pct != lastDlPct {
				lastDlPct = pct
				fmt.Fprintf(os.Stderr, "\r[mirror] download progress: %d%%", pct)
			}
		}
	}); err != nil {
		fmt.Fprintln(os.Stderr)
		log.Fatalf("[mirror] download tarball: %v", err)
	}
	fmt.Fprintln(os.Stderr)
	log.Printf("[mirror] download complete")

	// Step 3: Upload tarball to {s3_prefix}/stable/{hash}/{platform}.tar.gz
	tarballURL := mc.S3Prefix + "/stable/" + metaVersion + "/vscode-" + mc.Platform + ".tar.gz"
	log.Printf("[mirror] uploading tarball to %s", tarballURL)
	var lastUpPct int64 = -1
	if err := s3UploadFile(mc, tarballURL, tarballPath, "application/gzip", func(written, total int64) {
		if total > 0 {
			pct := written * 100 / total
			if pct != lastUpPct {
				lastUpPct = pct
				fmt.Fprintf(os.Stderr, "\r[mirror] upload progress: %d%%", pct)
			}
		}
	}); err != nil {
		fmt.Fprintln(os.Stderr)
		log.Fatalf("[mirror] upload tarball: %v", err)
	}
	fmt.Fprintln(os.Stderr)
	log.Printf("[mirror] tarball uploaded")

	// Upload version tag file: {s3_prefix}/stable/{hash}/{name}
	// Content is the original metadata JSON.
	if metaName != "" {
		tagURL := mc.S3Prefix + "/stable/" + metaVersion + "/" + metaName
		log.Printf("[mirror] uploading version tag to %s", tagURL)
		if err := s3UploadBytes(mc, tagURL, metaJSON, "application/json", nil); err != nil {
			log.Fatalf("[mirror] upload version tag: %v", err)
		}
		log.Printf("[mirror] version tag uploaded")
	}

	// Remove "url" field from metadata JSON before uploading to S3.
	// The cache server does not provide a download url; the download path is
	// constructed from {s3_prefix}/stable/{hash}/vscode-{platform}.tar.gz
	delete(meta, "url")
	cachedJSON, err := json.Marshal(meta)
	if err != nil {
		log.Fatalf("[mirror] marshal cached JSON: %v", err)
	}

	// Step 4: Upload latest JSON (latest mode only).
	if version == "latest" {
		latestURL := mc.S3Prefix + "/api/latest/" + mc.Platform + "/stable"
		log.Printf("[mirror] uploading latest JSON to %s", latestURL)
		if err := s3UploadBytes(mc, latestURL, cachedJSON, "application/json", nil); err != nil {
			log.Fatalf("[mirror] upload latest JSON: %v", err)
		}
		log.Printf("[mirror] latest JSON uploaded")
	}

	// Step 5: Upload version JSON.
	versionURL := mc.S3Prefix + "/api/versions/" + metaName + "/" + mc.Platform + "/stable"
	log.Printf("[mirror] uploading version JSON to %s", versionURL)
	if err := s3UploadBytes(mc, versionURL, cachedJSON, "application/json", nil); err != nil {
		log.Fatalf("[mirror] upload version JSON: %v", err)
	}
	log.Printf("[mirror] version JSON uploaded")

	log.Printf("[mirror] done: version %s (hash %s)", metaName, metaVersion)
}
