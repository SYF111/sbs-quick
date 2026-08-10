package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var webUI embed.FS

type Job struct {
	ID       string  `json:"id"`
	Filename string  `json:"filename"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Msg      string  `json:"msg,omitempty"`
	Error    string  `json:"error,omitempty"`
	OutID    string  `json:"out_id,omitempty"`
	Elapsed  float64 `json:"elapsed"`
}

var (
	jobs    = map[string]*Job{}
	jobsMu  sync.Mutex
	jobSeq  int
	ffmpeg_ string
	outputs = map[string]string{} // outID → file path
	outsMu  sync.Mutex
)

func main() {
	port := "7878"
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--port=") {
		port = strings.TrimPrefix(os.Args[1], "--port=")
	}

	ffmpeg_ = findFFmpeg()

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveUI)
	mux.HandleFunc("/api/single", handleSingle)
	mux.HandleFunc("/api/jobs", handleJobs)
	mux.HandleFunc("/api/stats", handleStats)
	mux.HandleFunc("/api/download/", handleDownload)

	basePort, _ := strconv.Atoi(port)
	for i := 0; i < 10; i++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", basePort+i))
		if err == nil {
			ln.Close()
			port = fmt.Sprintf("%d", basePort+i)
			break
		}
	}

	url := fmt.Sprintf("http://127.0.0.1:%s", port)
	log.Printf("SBS Quick — %s", url)
	log.Printf("ffmpeg: %s", ffmpeg_)
	if ffmpeg_ == "" { log.Println("WARNING: ffmpeg not found") }
	log.Println("Press Ctrl+C to stop (关闭此窗口退出)")
	openBrowser(url)

	if err := http.ListenAndServe("127.0.0.1:"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func findFFmpeg() string {
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		if err := exec.Command("ffmpeg", "-version").Run(); err == nil {
			return "ffmpeg"
		}
	}
	home, _ := os.UserHomeDir()
	dirs := []string{"/usr/local/bin", "/opt/homebrew/bin"}
	if runtime.GOOS == "windows" {
		dirs = []string{
			filepath.Join(os.Getenv("ProgramFiles"), "ffmpeg", "bin"),
			`C:\ffmpeg\bin`,
			`C:\Program Files\ffmpeg\bin`,
		}
	}
	for _, d := range dirs {
		p := filepath.Join(d, "ffmpeg"+ext())
		if _, err := os.Stat(p); err == nil {
			if exec.Command(p, "-version").Run() == nil { return p }
		}
	}
	// Winget deep search
	if runtime.GOOS == "windows" {
		root := filepath.Join(home, `AppData\Local\Microsoft\WinGet\Packages`)
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil { return filepath.SkipDir }
			if d.IsDir() && depth(p) > 5 { return filepath.SkipDir }
			if strings.EqualFold(d.Name(), "ffmpeg.exe") {
				if exec.Command(p, "-version").Run() == nil {
					dirs = append(dirs, filepath.Dir(p))
				}
			}
			return nil
		})
		for _, d := range dirs {
			p := filepath.Join(d, "ffmpeg.exe")
			if _, err := os.Stat(p); err == nil { return p }
		}
	}
	return ""
}

func ext() string {
	if runtime.GOOS == "windows" { return ".exe" }
	return ""
}

func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin": c = exec.Command("open", url)
	case "windows": c = exec.Command("cmd", "/c", "start", url)
	default: c = exec.Command("xdg-open", url)
	}
	c.Start()
}

// ── HTTP handlers ─────────────────────────────────────────

func serveUI(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.ParseFS(webUI, "index.html")
	tmpl.Execute(w, nil)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ffmpeg": ffmpeg_ != "", "os": runtime.GOOS, "arch": runtime.GOARCH,
	})
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	jobsMu.Lock(); defer jobsMu.Unlock()
	list := make([]*Job, 0, len(jobs))
	for _, j := range jobs { list = append(list, j) }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/download/")
	outsMu.Lock()
	path, ok := outputs[id]
	outsMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(path)))
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeFile(w, r, path)
}

// ── Single file processing (multipart upload) ──────────────

func handleSingle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { http.Error(w, "POST only", 405); return }

	if err := r.ParseMultipartForm(2 << 30); err != nil { // 2GB max
		http.Error(w, err.Error(), 400)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		http.Error(w, "missing video file", 400)
		return
	}
	defer file.Close()

	shift, _ := strconv.Atoi(r.FormValue("shift"))
	if shift < 1 { shift = 6 }
	compress, _ := strconv.ParseFloat(r.FormValue("compress"), 64)
	if compress < 0 { compress = 0 }
	maxSize, _ := strconv.Atoi(r.FormValue("max_size"))
	if maxSize < 1 { maxSize = 1280 }
	batch := r.FormValue("batch") == "1"

	filename := header.Filename
	if filename == "" { filename = "video.mp4" }
	job := newJob(filename)

	go func() {
		// Save uploaded file (keep temp dir — output is served from here)
		tmpDir, _ := os.MkdirTemp("", "sbs_quick_")
		inPath := filepath.Join(tmpDir, filename)
		f, _ := os.Create(inPath)
		io.Copy(f, file)
		f.Close()

		if batch {
			// For batch: just queue this one; caller manages batch orchestration
			runSingle(job, inPath, shift, compress, maxSize)
		} else {
			runSingle(job, inPath, shift, compress, maxSize)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

// ── Core processing ────────────────────────────────────────

func runSingle(job *Job, inPath string, shift int, compress float64, maxSize int) {
	start := time.Now()
	updateJob(job.ID, "running", 0, "准备中...", "")

	if ffmpeg_ == "" {
		updateJob(job.ID, "error", 0, "ffmpeg 未安装。请在 PowerShell 运行: winget install ffmpeg", "")
		return
	}

	// Build ffmpeg filter for: scale → compress → shift → hstack → encode
	var filters []string

	// Scale if needed
	probeW, probeH := probeVideo(inPath)
	if probeW > maxSize || probeH > maxSize {
		scale := float64(maxSize) / float64(max(probeW, probeH))
		filters = append(filters, fmt.Sprintf("scale=trunc(iw*%f/2)*2:trunc(ih*%f/2)*2", scale, scale))
	}

	// Split
	filters = append(filters, "split[l][r]")

	// Left eye: compress horizontally, then shift right
	filters = append(filters, buildCompressShift("l", "lc", compress, shift))
	// Right eye: compress horizontally, then shift left
	filters = append(filters, buildCompressShift("r", "rc", compress, -shift))

	// hstack
	filters = append(filters, "[lc][rc]hstack[out]")

	filterSpec := strings.Join(filters, ";")

	tmpDir := filepath.Dir(inPath)
	base := strings.TrimSuffix(filepath.Base(inPath), filepath.Ext(inPath))
	outPath := filepath.Join(tmpDir, base+"_quick_sbs.mp4")
	for n := 1; fileExists(outPath); n++ {
		outPath = filepath.Join(tmpDir, fmt.Sprintf("%s_quick_sbs_%d.mp4", base, n))
	}

	updateJob(job.ID, "running", 0.05, "编码中...", "")

	cmd := exec.Command(ffmpeg_,
		"-y", "-i", inPath,
		"-filter_complex", filterSpec,
		"-map", "[out]", "-map", "0:a?",
		"-c:v", "libx264", "-crf", "18", "-preset", "fast",
		"-pix_fmt", "yuv420p", "-c:a", "aac",
		"-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats",
		outPath,
	)

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stderrB := new(strings.Builder)

	if err := cmd.Start(); err != nil {
		updateJob(job.ID, "error", 0, err.Error(), "")
		return
	}
	go func() { io.Copy(stderrB, stderr) }()

	// Progress parsing
	dur := probeDuration(inPath)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if err != nil { break }
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				if strings.HasPrefix(line, "out_time=") {
					secs := parseTime(strings.TrimPrefix(line, "out_time="))
					if dur > 0 {
						p := secs / dur
						if p < 0.05 { p = 0.05 }
						if p > 0.99 { p = 0.99 }
						updateJob(job.ID, "running", p, fmt.Sprintf("处理中 %.0f%%", p*100), "")
					}
				}
			}
		}
	}()

	err := cmd.Wait()
	elapsed := time.Since(start).Seconds()

	if err != nil {
		msg := err.Error()
		if s := stderrB.String(); s != "" {
			msg += "\n" + lastLines(s, 3)
		}
		updateJob(job.ID, "error", 0, msg, "")
		log.Printf("[%s] FAIL: %v (%.1fs)", job.ID, err, elapsed)
		return
	}

	// Register output for download
	outID := fmt.Sprintf("%s_%d", job.ID, time.Now().UnixNano())
	outsMu.Lock()
	outputs[outID] = outPath
	outsMu.Unlock()

	updateJob(job.ID, "done", 1.0, fmt.Sprintf("完成 (%.0f秒)", elapsed), outID)
	log.Printf("[%s] DONE: %s (%.1fs)", job.ID, outPath, elapsed)
}

func buildCompressShift(srcLabel, dstLabel string, compress float64, shiftPx int) string {
	if compress > 0 {
		// Scale narrower, then pad back to original width (centered)
		return fmt.Sprintf(
			"[%s]scale=trunc(iw*(1-%f)/2)*2:ih,pad=iw:ih:(iw-ow)/2:0,"+
				"crop=iw-%d:ih:%d:0,pad=iw:ih:%d:0[%s]",
			srcLabel, compress,
			abs(shiftPx), max(shiftPx, 0), max(shiftPx, 0),
			dstLabel,
		)
	} else {
		if shiftPx >= 0 {
			return fmt.Sprintf("[%s]crop=iw-%d:ih:%d:0,pad=iw:ih:%d:0[%s]",
				srcLabel, shiftPx, shiftPx, shiftPx, dstLabel)
		} else {
			s := -shiftPx
			return fmt.Sprintf("[%s]crop=iw-%d:ih:0:0,pad=iw:ih:%d:0[%s]",
				srcLabel, s, s, dstLabel)
		}
	}
}

// ── Helpers ────────────────────────────────────────────────

func newJob(filename string) *Job {
	jobsMu.Lock(); defer jobsMu.Unlock()
	jobSeq++
	j := &Job{ID: strconv.Itoa(jobSeq), Filename: filename, Status: "queued"}
	jobs[j.ID] = j
	return j
}

func updateJob(id, status string, progress float64, msg, outID string) {
	jobsMu.Lock(); defer jobsMu.Unlock()
	if j, ok := jobs[id]; ok {
		j.Status = status; j.Progress = progress; j.Msg = msg; j.OutID = outID
	}
}

func probeVideo(path string) (int, int) {
	cmd := exec.Command(ffmpeg_, "-i", path, "-f", "null", "-")
	// ffmpeg prints stream info to stderr
	out, _ := cmd.CombinedOutput()
	w, h := 1920, 1080
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Stream") && strings.Contains(line, "Video") {
			// Parse "1280x720" or similar
			idx := strings.Index(line, ",")
			if idx < 0 { continue }
			dims := strings.Split(strings.TrimSpace(line[:idx]), " ")
			last := dims[len(dims)-1]
			parts := strings.Split(last, "x")
			if len(parts) == 2 {
				w, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
				h, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
			}
			break
		}
		// Alternate: "Video: h264 ... 1280x720"
		for _, word := range strings.Fields(line) {
			if strings.Count(word, "x") == 1 {
				parts := strings.Split(word, "x")
				ww, err1 := strconv.Atoi(parts[0])
				hh, err2 := strconv.Atoi(parts[1])
				if err1 == nil && err2 == nil && ww > 0 && hh > 0 {
					w, h = ww, hh
					break
				}
			}
		}
	}
	return w, h
}

func probeDuration(path string) float64 {
	cmd := exec.Command(ffmpeg_, "-i", path, "-f", "null", "-")
	out, _ := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Duration") {
			fields := strings.Fields(line)
			for _, f := range fields {
				if strings.Count(f, ":") == 2 {
					return parseTime(f)
				}
			}
		}
	}
	return 60
}

func parseTime(ts string) float64 {
	ts = strings.TrimSuffix(ts, ",")
	var h, m int; var s float64
	fmt.Sscanf(ts, "%d:%d:%f", &h, &m, &s)
	return float64(h*3600+m*60) + s
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func depth(p string) int {
	home, _ := os.UserHomeDir()
	return len(strings.Split(strings.TrimPrefix(p, home), string(os.PathSeparator)))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n { return s }
	return strings.Join(lines[len(lines)-n:], "\n")
}

func abs(x int) int { if x < 0 { return -x }; return x }
