package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	certFileName        = "cert.pem"
	keyFileName         = "key.pem"
	sessionExpiry       = 24 * time.Hour
	timeDisplayLayout   = "%d年%2d月%2d日 %2d:%02d:%02d %s"
	uploadFieldName     = "files"
	uploadPathFieldName = "paths"
	uploadDirFieldName  = "dirs"
)

var pageTmpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}}</title>
  <style>
    :root {
      color-scheme: light;
      font-family: "SFMono-Regular", "Consolas", "Liberation Mono", monospace;
      background: #f4f7fb;
      color: #142033;
    }
    * {
      box-sizing: border-box;
    }
    body {
      margin: 0;
      min-height: 100vh;
      background:
        radial-gradient(circle at top left, rgba(206, 226, 247, 0.45), transparent 30rem),
        linear-gradient(180deg, #f6f9fc 0%, #edf3f8 100%);
    }
    main {
      width: min(100%, 72rem);
      margin: 0 auto;
      padding: 1rem;
    }
    .list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }
    .row {
      display: grid;
      grid-template-columns: minmax(0, 1fr) minmax(5rem, 7rem) minmax(13rem, 18rem);
      gap: 1rem;
      align-items: center;
      width: 100%;
      padding: 0.9rem 1rem;
      border: 0;
      border-radius: 0.9rem;
      text-decoration: none;
      color: inherit;
      text-align: left;
      background: #f8fbfe;
      box-shadow: inset 0 0 0 1px rgba(20, 32, 51, 0.05);
    }
    .tone-a {
      background: #f7fafc;
    }
    .tone-b {
      background: #edf3f9;
    }
    .upload-row {
      background: #ffffff;
      cursor: pointer;
    }
    .upload-row.dragover {
      box-shadow: inset 0 0 0 2px rgba(56, 104, 178, 0.25);
      background: #f9fcff;
    }
    .diff-row {
      position: relative;
      background: #e9edf2;
      align-items: start;
    }
    .hidden {
      display: none;
    }
    .name,
    .size,
    .modified {
      min-width: 0;
      text-align: left;
      word-break: break-all;
    }
    .size,
    .modified {
      white-space: pre;
    }
    .diff-text {
      grid-column: 1 / -1;
      margin: 0;
      line-height: 1.5;
    }
    .diff-list {
      margin: 0.35rem 0 0;
      padding-left: 1.4rem;
    }
    .diff-confirm {
      position: absolute;
      top: 0.7rem;
      right: 0.8rem;
      border: 0;
      background: transparent;
      cursor: pointer;
      font: inherit;
      padding: 0;
    }
    @media (max-width: 720px) {
      main {
        padding: 0.75rem;
      }
      .row {
        gap: 0.65rem;
        grid-template-columns: minmax(0, 1fr) minmax(4.5rem, 6rem);
      }
      .modified {
        grid-column: 1 / -1;
      }
    }
  </style>
</head>
<body>
  <main>
    <div class="list">
      {{range .Rows}}
      <a class="row {{.Tone}}" href="{{.Href}}">
        <span class="name">{{.Name}}</span>
        <span class="size">{{.Size}}</span>
        <span class="modified">{{.Modified}}</span>
      </a>
      {{end}}
      <button id="uploadRow" class="row upload-row" type="button">
        <span class="name">上传文件</span>
        <span class="size"></span>
        <span class="modified"></span>
      </button>
      <div id="diffRow" class="row diff-row hidden">
        <button id="confirmButton" class="diff-confirm" type="button">✅</button>
        <div class="diff-text">
          <div>以下文件会被覆盖:</div>
          <ul id="diffList" class="diff-list"></ul>
        </div>
      </div>
    </div>
  </main>
  <input id="fileInput" type="file" multiple hidden>
  <script>
    (() => {
      const uploadRow = document.getElementById('uploadRow');
      const fileInput = document.getElementById('fileInput');
      const diffRow = document.getElementById('diffRow');
      const diffList = document.getElementById('diffList');
      const confirmButton = document.getElementById('confirmButton');
      let pendingSessionId = '';

      const showDiff = (paths, sessionId) => {
        pendingSessionId = sessionId;
        for (const item of paths) {
          const li = document.createElement('li');
          li.textContent = item;
          diffList.appendChild(li);
        }
        diffRow.classList.remove('hidden');
        requestAnimationFrame(scrollForDiff);
      };

      const hideDiff = () => {
        pendingSessionId = '';
        diffList.textContent = '';
        diffRow.classList.add('hidden');
      };

      const scrollForDiff = () => {
        const viewportHeight = window.innerHeight || document.documentElement.clientHeight;
        const uploadRect = uploadRow.getBoundingClientRect();
        const diffRect = diffRow.getBoundingClientRect();
        const maxDown = Math.max(0, uploadRect.top);
        const needDown = Math.max(0, diffRect.bottom - viewportHeight);
        const delta = Math.min(maxDown, needDown);
        if (delta > 0) {
          window.scrollBy({ top: delta, behavior: 'smooth' });
        }
      };

      const uploadItems = async (payload) => {
        if (!payload.files.length && !payload.dirs.length) {
          return;
        }
        hideDiff();
        const form = new FormData();
        for (const dir of payload.dirs) {
          form.append('dirs', dir);
        }
        for (const item of payload.files) {
          form.append('paths', item.path);
          form.append('files', item.file, item.file.name);
        }
        const response = await fetch('/__upload?path=' + encodeURIComponent(window.location.pathname), {
          method: 'POST',
          body: form,
        });
        if (!response.ok) {
          throw new Error(await response.text() || 'upload failed');
        }
        const result = await response.json();
        if (result.committed) {
          window.location.reload();
          return;
        }
        showDiff(result.conflicts || [], result.sessionId || '');
      };

      const collectFromFileInput = () => {
        return {
          files: Array.from(fileInput.files || []).map((file) => ({
            file,
            path: file.webkitRelativePath || file.name,
          })),
          dirs: [],
        };
      };

      const walkHandle = async (handle, prefix, files, dirs) => {
        const nextPath = prefix ? prefix + '/' + handle.name : handle.name;
        if (handle.kind === 'file') {
          files.push({ file: await handle.getFile(), path: nextPath });
          return;
        }
        dirs.push(nextPath);
        for await (const child of handle.values()) {
          await walkHandle(child, nextPath, files, dirs);
        }
      };

      const readEntries = async (reader) => {
        return new Promise((resolve, reject) => reader.readEntries(resolve, reject));
      };

      const walkEntry = async (entry, prefix, files, dirs) => {
        const nextPath = prefix ? prefix + '/' + entry.name : entry.name;
        if (entry.isFile) {
          const file = await new Promise((resolve, reject) => entry.file(resolve, reject));
          files.push({ file, path: nextPath });
          return;
        }
        if (!entry.isDirectory) {
          return;
        }
        dirs.push(nextPath);
        const reader = entry.createReader();
        while (true) {
          const batch = await readEntries(reader);
          if (!batch.length) {
            return;
          }
          for (const child of batch) {
            await walkEntry(child, nextPath, files, dirs);
          }
        }
      };

      const collectFromDrop = async (event) => {
        const files = [];
        const dirs = [];
        const items = Array.from(event.dataTransfer?.items || []);
        if (items.length && items.some((item) => item.getAsFileSystemHandle || item.webkitGetAsEntry)) {
          for (const item of items) {
            if (item.kind !== 'file') {
              continue;
            }
            if (item.getAsFileSystemHandle) {
              const handle = await item.getAsFileSystemHandle();
              if (handle) {
                await walkHandle(handle, '', files, dirs);
              }
              continue;
            }
            if (item.webkitGetAsEntry) {
              const entry = item.webkitGetAsEntry();
              if (entry) {
                await walkEntry(entry, '', files, dirs);
              }
              continue;
            }
            const file = item.getAsFile();
            if (file) {
              files.push({ file, path: file.name });
            }
          }
          return { files, dirs };
        }
        for (const file of Array.from(event.dataTransfer?.files || [])) {
          files.push({ file, path: file.webkitRelativePath || file.name });
        }
        return { files, dirs };
      };

      uploadRow.addEventListener('click', () => fileInput.click());
      fileInput.addEventListener('change', async () => {
        try {
          await uploadItems(collectFromFileInput());
        } catch (error) {
          console.error(error);
          alert(String(error.message || error));
        } finally {
          fileInput.value = '';
        }
      });
      uploadRow.addEventListener('dragover', (event) => {
        event.preventDefault();
        uploadRow.classList.add('dragover');
      });
      uploadRow.addEventListener('dragleave', () => {
        uploadRow.classList.remove('dragover');
      });
      uploadRow.addEventListener('drop', async (event) => {
        event.preventDefault();
        uploadRow.classList.remove('dragover');
        try {
          await uploadItems(await collectFromDrop(event));
        } catch (error) {
          console.error(error);
          alert(String(error.message || error));
        }
      });
      confirmButton.addEventListener('click', async () => {
        if (!pendingSessionId) {
          return;
        }
        const response = await fetch('/__commit', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ sessionId: pendingSessionId }),
        });
        if (!response.ok) {
          alert(await response.text() || 'commit failed');
          return;
        }
        window.location.reload();
      });
    })();
  </script>
</body>
</html>
`))

type server struct {
	root      string
	stageRoot string

	mu       sync.Mutex
	sessions map[string]*uploadSession
}

type uploadSession struct {
	ID        string
	TargetRel string
	StageDir  string
	CreatedAt time.Time
}

type rowData struct {
	Name     string
	Href     string
	Size     string
	Modified string
	Tone     string
}

type pageData struct {
	Title string
	Rows  []rowData
}

type uploadResponse struct {
	Committed bool     `json:"committed"`
	SessionID string   `json:"sessionId,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}

type commitRequest struct {
	SessionID string `json:"sessionId"`
}

type requiredStringFlag struct {
	value string
	set   bool
}

func (f *requiredStringFlag) String() string {
	if !f.set {
		return ""
	}
	return f.value
}

func (f *requiredStringFlag) Set(value string) error {
	f.value = value
	f.set = true
	return nil
}

type requiredIntFlag struct {
	value int
	set   bool
}

func (f *requiredIntFlag) String() string {
	if !f.set {
		return ""
	}
	return strconv.Itoa(f.value)
}

func (f *requiredIntFlag) Set(value string) error {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	f.value = parsed
	f.set = true
	return nil
}

func main() {
	log.SetFlags(0)

	var port requiredIntFlag
	var publicDir requiredStringFlag
	flag.Var(&port, "p", "TCP `PORT`")
	flag.Var(&publicDir, "d", "要发布的 `PUBLIC_DIRECTORY`")
	httpsDir := flag.String("s", "", "使用 `HTTPS_DIRECTORY` 下的证书")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "用法: %s -p PORT -d PUBLIC_DIRECTORY [-s HTTPS_DIRECTORY]\n", filepath.Base(os.Args[0]))
		fmt.Fprintln(out)
		flag.PrintDefaults()
	}
	flag.Parse()

	var missing []string
	if !publicDir.set || publicDir.value == "" {
		missing = append(missing, "-d")
	}
	if !port.set {
		missing = append(missing, "-p")
	}
	if len(missing) > 0 {
		fmt.Fprintf(flag.CommandLine.Output(), "缺少必填参数: %s\n\n", strings.Join(missing, ", "))
		flag.Usage()
		os.Exit(2)
	}
	if port.value <= 0 || port.value > 65535 {
		log.Fatalf("无效端口: %d", port.value)
	}

	root, err := filepath.Abs(publicDir.value)
	if err != nil {
		log.Fatalf("解析目录失败: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		log.Fatalf("读取目录失败: %v", err)
	}
	if !info.IsDir() {
		log.Fatalf("不是目录: %s", root)
	}

	stageRoot, err := os.MkdirTemp("", "receiver-stage-*")
	if err != nil {
		log.Fatalf("创建暂存目录失败: %v", err)
	}
	defer os.RemoveAll(stageRoot)

	certDir := *httpsDir
	removeCertDir := false
	if certDir == "" {
		certDir, err = os.MkdirTemp("", "receiver-cert-*")
		if err != nil {
			log.Fatalf("创建证书目录失败: %v", err)
		}
		removeCertDir = true
	}
	if removeCertDir {
		defer os.RemoveAll(certDir)
	}
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		log.Fatalf("创建证书目录失败: %v", err)
	}

	certPath := filepath.Join(certDir, certFileName)
	keyPath := filepath.Join(certDir, keyFileName)
	if err := ensureCertificate(certPath, keyPath); err != nil {
		log.Fatalf("准备证书失败: %v", err)
	}

	srv := &server{
		root:      root,
		stageRoot: stageRoot,
		sessions:  make(map[string]*uploadSession),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/__upload", srv.handleUpload)
	mux.HandleFunc("/__commit", srv.handleCommit)
	mux.HandleFunc("/", srv.handleBrowse)

	addr := fmt.Sprintf(":%d", port.value)
	log.Printf("https://localhost:%d/", port.value)
	log.Fatal(http.ListenAndServeTLS(addr, certPath, keyPath, mux))
}

func ensureCertificate(certPath, keyPath string) error {
	if fileExists(certPath) && fileExists(keyPath) {
		return nil
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	hostname, _ := os.Hostname()
	dnsNames := []string{"localhost"}
	if hostname != "" && hostname != "localhost" {
		dnsNames = append(dnsNames, hostname)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return err
	}

	if err := writePEM(certPath, "CERTIFICATE", derBytes, 0o644); err != nil {
		return err
	}

	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return err
	}
	return writePEM(keyPath, "PRIVATE KEY", keyBytes, 0o600)
}

func writePEM(filePath, blockType string, bytes []byte, mode fs.FileMode) error {
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	return pem.Encode(file, &pem.Block{Type: blockType, Bytes: bytes})
}

func (s *server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rel := cleanRequestPath(r.URL.Path)
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if info.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			target := encodedPath(rel, true)
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		s.renderDirectory(w, r, rel, abs)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, encodedPath(rel, false), http.StatusMovedPermanently)
		return
	}
	servePlainFile(w, r, abs, info)
}

func servePlainFile(w http.ResponseWriter, r *http.Request, filePath string, info os.FileInfo) {
	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer file.Close()

	if isTextFile(filePath, file) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func isTextFile(filePath string, file *os.File) bool {
	if isTextExtension(filepath.Ext(filePath)) {
		return true
	}

	buffer := make([]byte, 8192)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return false
	}
	return isTextSample(buffer[:n])
}

func isTextExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case ".bat", ".c", ".cc", ".cfg", ".conf", ".cpp", ".cs", ".css", ".csv", ".go", ".h", ".hpp", ".htm", ".html", ".ini", ".java", ".js", ".json", ".jsx", ".log", ".lua", ".md", ".mjs", ".py", ".rb", ".rs", ".sh", ".sql", ".svg", ".toml", ".ts", ".tsx", ".txt", ".xml", ".yaml", ".yml":
		return true
	default:
		return false
	}
}

func isTextSample(sample []byte) bool {
	if len(sample) == 0 {
		return true
	}
	if !utf8.Valid(sample) {
		return false
	}
	for _, b := range sample {
		if b == 0 {
			return false
		}
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' {
			return false
		}
	}
	return true
}

func (s *server) renderDirectory(w http.ResponseWriter, r *http.Request, rel, abs string) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var dirs, files []rowData
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		name := entry.Name()
		childRel := joinRel(rel, name)
		row := rowData{
			Name:     name,
			Href:     encodedPath(childRel, entry.IsDir()),
			Size:     "",
			Modified: formatModifiedTime(info.ModTime()),
		}
		if entry.IsDir() {
			row.Name += "/"
			dirs = append(dirs, row)
			continue
		}
		row.Size = formatSize(info.Size())
		files = append(files, row)
	}

	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	rows := make([]rowData, 0, 1+len(dirs)+len(files))
	parentRel := parentRelPath(rel)
	rows = append(rows, rowData{Name: "..", Href: encodedPath(parentRel, true)})
	rows = append(rows, dirs...)
	rows = append(rows, files...)
	for i := range rows {
		if i%2 == 0 {
			rows[i].Tone = "tone-a"
			continue
		}
		rows[i].Tone = "tone-b"
	}

	title := "/"
	if rel != "" {
		title = "/" + rel + "/"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, pageData{Title: title, Rows: rows}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	targetRel := cleanRequestPath(r.URL.Query().Get("path"))
	targetAbs := filepath.Join(s.root, filepath.FromSlash(targetRel))
	targetInfo, err := os.Stat(targetAbs)
	if err != nil || !targetInfo.IsDir() {
		http.Error(w, "invalid target directory", http.StatusBadRequest)
		return
	}

	stageDir, err := os.MkdirTemp(s.stageRoot, "upload-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageDir)
		}
	}()

	if err := buildStageFromRequest(r.Context(), r, stageDir); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	hasEntries, err := directoryHasEntries(stageDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !hasEntries {
		http.Error(w, "empty upload", http.StatusBadRequest)
		return
	}

	conflicts, err := collectConflicts(targetAbs, stageDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(conflicts) == 0 {
		if err := applyStage(targetAbs, stageDir); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, uploadResponse{Committed: true})
		return
	}

	sessionID, err := randomID()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.cleanupExpiredSessionsLocked(time.Now())
	s.sessions[sessionID] = &uploadSession{
		ID:        sessionID,
		TargetRel: targetRel,
		StageDir:  stageDir,
		CreatedAt: time.Now(),
	}
	s.mu.Unlock()

	cleanupStage = false
	writeJSON(w, uploadResponse{
		Committed: false,
		SessionID: sessionID,
		Conflicts: conflicts,
	})
}

func (s *server) handleCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req commitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.cleanupExpiredSessionsLocked(time.Now())
	session, ok := s.sessions[req.SessionID]
	if ok {
		delete(s.sessions, req.SessionID)
	}
	s.mu.Unlock()

	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	defer os.RemoveAll(session.StageDir)

	targetAbs := filepath.Join(s.root, filepath.FromSlash(session.TargetRel))
	if err := applyStage(targetAbs, session.StageDir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) cleanupExpiredSessionsLocked(now time.Time) {
	for id, session := range s.sessions {
		if now.Sub(session.CreatedAt) <= sessionExpiry {
			continue
		}
		_ = os.RemoveAll(session.StageDir)
		delete(s.sessions, id)
	}
}

func buildStageFromRequest(ctx context.Context, r *http.Request, stageDir string) error {
	reader, err := r.MultipartReader()
	if err != nil {
		return err
	}
	var pendingPaths []string

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		part, err := reader.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		partErr := func() error {
			defer part.Close()

			switch part.FormName() {
			case uploadPathFieldName:
				bytes, err := io.ReadAll(part)
				if err != nil {
					return err
				}
				rel, err := cleanUploadPath(string(bytes))
				if err != nil {
					return err
				}
				pendingPaths = append(pendingPaths, rel)
				return nil
			case uploadFieldName:
				if len(pendingPaths) == 0 {
					return errors.New("missing upload path")
				}
				rel := pendingPaths[0]
				pendingPaths = pendingPaths[1:]
				return writePartToStage(part, stageDir, rel)
			case uploadDirFieldName:
				bytes, err := io.ReadAll(part)
				if err != nil {
					return err
				}
				rel, err := cleanUploadPath(string(bytes))
				if err != nil {
					return err
				}
				return os.MkdirAll(filepath.Join(stageDir, filepath.FromSlash(rel)), 0o755)
			default:
				_, err := io.Copy(io.Discard, part)
				return err
			}
		}()
		if partErr != nil {
			return partErr
		}
	}
}

func writePartToStage(reader io.Reader, stageDir, rel string) error {
	target := filepath.Join(stageDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, reader)
	return err
}

func directoryHasEntries(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

func collectConflicts(targetDir, stageDir string) ([]string, error) {
	set := make(map[string]struct{})
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		stagePath := filepath.Join(stageDir, entry.Name())
		targetPath := filepath.Join(targetDir, entry.Name())
		if err := collectEntryConflicts(stagePath, targetPath, entry.Name(), set); err != nil {
			return nil, err
		}
	}

	conflicts := make([]string, 0, len(set))
	for item := range set {
		conflicts = append(conflicts, item)
	}
	sort.Strings(conflicts)
	return conflicts, nil
}

func collectEntryConflicts(stagePath, targetPath, rel string, set map[string]struct{}) error {
	stageInfo, err := os.Lstat(stagePath)
	if err != nil {
		return err
	}
	targetInfo, err := os.Lstat(targetPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	rel = filepath.ToSlash(rel)
	if !stageInfo.IsDir() {
		if targetInfo.IsDir() {
			return collectExistingPaths(targetPath, rel, set, true)
		}
		set[rel] = struct{}{}
		return nil
	}

	if !targetInfo.IsDir() {
		set[rel] = struct{}{}
		return nil
	}

	entries, err := os.ReadDir(stagePath)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		childRel := filepath.Join(rel, entry.Name())
		if err := collectEntryConflicts(filepath.Join(stagePath, entry.Name()), filepath.Join(targetPath, entry.Name()), childRel, set); err != nil {
			return err
		}
	}
	return nil
}

func collectExistingPaths(existingPath, rel string, set map[string]struct{}, includeSelfIfEmpty bool) error {
	info, err := os.Lstat(existingPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		set[filepath.ToSlash(rel)] = struct{}{}
		return nil
	}

	found := false
	err = filepath.WalkDir(existingPath, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		found = true
		sub, err := filepath.Rel(existingPath, current)
		if err != nil {
			return err
		}
		targetRel := filepath.ToSlash(filepath.Join(rel, sub))
		set[targetRel] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if !found && includeSelfIfEmpty {
		set[filepath.ToSlash(rel)+"/"] = struct{}{}
	}
	return nil
}

func applyStage(targetDir, stageDir string) error {
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := applyStageEntry(filepath.Join(stageDir, entry.Name()), filepath.Join(targetDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func applyStageEntry(stagePath, targetPath string) error {
	info, err := os.Lstat(stagePath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if existing, err := os.Lstat(targetPath); err == nil {
			if !existing.IsDir() {
				if err := os.RemoveAll(targetPath); err != nil {
					return err
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		if err := os.MkdirAll(targetPath, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(stagePath)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := applyStageEntry(filepath.Join(stagePath, entry.Name()), filepath.Join(targetPath, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}

	if err := os.RemoveAll(targetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}

	src, err := os.Open(stagePath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func randomID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func cleanRequestPath(raw string) string {
	cleaned := path.Clean("/" + raw)
	if cleaned == "/" {
		return ""
	}
	return strings.TrimPrefix(cleaned, "/")
}

func cleanUploadPath(raw string) (string, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" {
		return "", errors.New("empty upload path")
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", errors.New("invalid upload path")
		}
	}
	cleaned := path.Clean("/" + raw)
	rel := strings.TrimPrefix(cleaned, "/")
	if rel == "" || rel == "." {
		return "", errors.New("invalid upload path")
	}
	return rel, nil
}

func joinRel(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "/" + child
}

func parentRelPath(rel string) string {
	if rel == "" {
		return ""
	}
	parent := path.Dir(rel)
	if parent == "." {
		return ""
	}
	return parent
}

func encodedPath(rel string, isDir bool) string {
	if rel == "" {
		return "/"
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	joined := "/" + strings.Join(parts, "/")
	if isDir {
		return joined + "/"
	}
	return joined
}

func formatSize(size int64) string {
	type unit struct {
		label string
		value float64
	}
	units := []unit{
		{label: "G", value: 1024 * 1024 * 1024},
		{label: "M", value: 1024 * 1024},
		{label: "K", value: 1024},
		{label: "B", value: 1},
	}
	value := float64(size)
	for _, unit := range units {
		if unit.label == "B" || value >= unit.value {
			scaled := value / unit.value
			if scaled == float64(int64(scaled)) {
				return fmt.Sprintf("%d%s", int64(scaled), unit.label)
			}
			return fmt.Sprintf("%.1f%s", scaled, unit.label)
		}
	}
	return "0B"
}

func formatModifiedTime(t time.Time) string {
	hour := t.Hour()
	suffix := "AM"
	if hour >= 12 {
		suffix = "PM"
	}
	displayHour := hour % 12
	if displayHour == 0 {
		displayHour = 12
	}
	return fmt.Sprintf(timeDisplayLayout, t.Year(), int(t.Month()), t.Day(), displayHour, t.Minute(), t.Second(), suffix)
}

func fileExists(filePath string) bool {
	info, err := os.Stat(filePath)
	return err == nil && !info.IsDir()
}
