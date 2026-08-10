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
	"sort"
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
	amfOK   = -1 // -1 unknown, 0 no, 1 yes
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
	// 1. PATH
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		if exec.Command(p, "-version").Run() == nil { return p }
	}

	home, _ := os.UserHomeDir()

	// Candidate dirs where ffmpeg.exe commonly lives
	var dirs []string
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		dirs = []string{
			filepath.Join(os.Getenv("ProgramFiles"), "ffmpeg", "bin"),
			`C:\ffmpeg\bin`,
			`C:\Program Files\ffmpeg\bin`,
			// winget shims (winget install Gyan.FFmpeg creates these)
			filepath.Join(localAppData, `Microsoft\WinGet\Links`),
			filepath.Join(home, `scoop\apps\ffmpeg\current\bin`),
			filepath.Join(home, `scoop\shims`),
			`C:\ProgramData\chocolatey\bin`,
		}
	} else {
		dirs = []string{"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin"}
	}
	for _, d := range dirs {
		p := filepath.Join(d, "ffmpeg"+ext())
		if _, err := os.Stat(p); err == nil {
			if exec.Command(p, "-version").Run() == nil { return p }
		}
	}

	// 2. Deep search inside winget package dirs (path can be very deep, so no
	// depth cutoff — the old depth>5 skip missed real installs).
	if runtime.GOOS == "windows" {
		root := filepath.Join(home, `AppData\Local\Microsoft\WinGet\Packages`)
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil { return filepath.SkipDir }
			if d.IsDir() { return nil }
			if strings.EqualFold(d.Name(), "ffmpeg.exe") {
				if exec.Command(p, "-version").Run() == nil {
					dirs = append(dirs, filepath.Dir(p))
					return filepath.SkipAll
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
		"amd": amfAvailable(),
	})
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	jobsMu.Lock(); defer jobsMu.Unlock()
	list := make([]*Job, 0, len(jobs))
	for _, j := range jobs { list = append(list, j) }
	// Map iteration order is random in Go — sort by job ID (submission order)
	// so the frontend can rely on list[len-1] being the latest job.
	sort.Slice(list, func(i, k int) bool {
		ii, _ := strconv.Atoi(list[i].ID)
		kk, _ := strconv.Atoi(list[k].ID)
		return ii < kk
	})
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

	// Read file into memory BEFORE the handler returns (multipart reader is
	// closed when the handler exits, so the goroutine can't read it later).
	filedata, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		http.Error(w, "failed to read upload: "+err.Error(), 400)
		return
	}

	shift, _ := strconv.Atoi(r.FormValue("shift"))
	if shift < 1 { shift = 6 }
	compress, _ := strconv.ParseFloat(r.FormValue("compress"), 64)
	if compress < 0 { compress = 0 }
	maxSize, _ := strconv.Atoi(r.FormValue("max_size"))
	// 0 = keep original resolution (no scaling)
	if maxSize < 0 { maxSize = 0 }
	batch := r.FormValue("batch") == "1"
	encoder := r.FormValue("encoder")
	if encoder != "amd" && encoder != "cpu" { encoder = "auto" }

	filename := header.Filename
	if filename == "" { filename = "video.mp4" }
	job := newJob(filename)

	go func() {
		tmpDir, _ := os.MkdirTemp("", "sbs_quick_")
		inPath := filepath.Join(tmpDir, filename)
		if err := os.WriteFile(inPath, filedata, 0644); err != nil {
			updateJob(job.ID, "error", 0, err.Error(), "")
			return
		}

		if batch {
			// For batch: just queue this one; caller manages batch orchestration
			runSingle(job, inPath, shift, compress, maxSize, encoder)
		} else {
			runSingle(job, inPath, shift, compress, maxSize, encoder)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

// ── Core processing ────────────────────────────────────────

func runSingle(job *Job, inPath string, shift int, compress float64, maxSize int, encoder string) {
	start := time.Now()
	updateJob(job.ID, "running", 0, "准备中...", "")

	if ffmpeg_ == "" {
		updateJob(job.ID, "error", 0, "ffmpeg 未安装。请在 PowerShell 运行: winget install ffmpeg", "")
		return
	}

	// Build ffmpeg filter for: scale → compress → shift → hstack → encode
	var filters []string

	// Scale if needed. Label the scaled output [s] so split can consume it.
	// (Previously the scale output was unlabeled and split[l][r] failed to bind.)
	// maxSize <= 0 means keep the original resolution — no scaling at all.
	inLabel := "[0:v]"
	probeW, probeH := probeVideo(inPath)
	if maxSize > 0 && (probeW > maxSize || probeH > maxSize) {
		scale := float64(maxSize) / float64(max(probeW, probeH))
		filters = append(filters, fmt.Sprintf("[0:v]scale=trunc(iw*%f/2)*2:trunc(ih*%f/2)*2[s]", scale, scale))
		inLabel = "[s]"
	}

	// Split
	filters = append(filters, inLabel+"split[l][r]")

	// Left eye: compress horizontally, then shift right
	filters = append(filters, buildCompressShift("l", "lc", compress, true, shift))
	// Right eye: compress horizontally, then shift left
	filters = append(filters, buildCompressShift("r", "rc", compress, false, shift))

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

	// Choose encoder: amd = AMD GPU hardware (h264_amf), cpu = lossless x264.
	// "auto" picks AMD if the driver exposes h264_amf, else falls back to CPU.
	encArgs, encName := buildEncodeArgs(encoder)
	updateJob(job.ID, "running", 0.05, "编码中 ("+encName+")...", "")

	err := runFFmpeg(job, inPath, outPath, filterSpec, encArgs)
	// If the hardware encoder failed (driver issue, unsupported format...),
	// fall back to CPU lossless so the job still completes.
	if err != nil && encName == "AMD硬件加速" {
		log.Printf("[%s] AMD encode failed, retrying with CPU lossless: %v", job.ID, err)
		encArgs, encName = cpuEncodeArgs()
		updateJob(job.ID, "running", 0.05, "AMD 编码失败，改用 CPU 无损重试...", "")
		err = runFFmpeg(job, inPath, outPath, filterSpec, encArgs)
	}
	elapsed := time.Since(start).Seconds()

	if err != nil {
		updateJob(job.ID, "error", 0, err.Error(), "")
		log.Printf("[%s] FAIL: %v (%.1fs)", job.ID, err, elapsed)
		return
	}

	// Register output for download
	outID := fmt.Sprintf("%s_%d", job.ID, time.Now().UnixNano())
	outsMu.Lock()
	outputs[outID] = outPath
	outsMu.Unlock()

	updateJob(job.ID, "done", 1.0, fmt.Sprintf("完成 (%.0f秒, %s)", elapsed, encName), outID)
	log.Printf("[%s] DONE: %s (%.1fs, %s)", job.ID, outPath, elapsed, encName)
}

// runFFmpeg executes the encode command, streams progress updates to the job,
// and returns a descriptive error (with the last stderr lines) on failure.
func runFFmpeg(job *Job, inPath, outPath, filterSpec string, encArgs []string) error {
	args := []string{
		"-y", "-i", inPath,
		"-filter_complex", filterSpec,
		"-map", "[out]", "-map", "0:a?",
	}
	args = append(args, encArgs...)
	args = append(args,
		"-pix_fmt", "yuv420p", "-c:a", "aac",
		"-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats",
		outPath,
	)
	cmd := exec.Command(ffmpeg_, args...)

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stderrB := new(strings.Builder)

	if err := cmd.Start(); err != nil {
		return err
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
	if err != nil {
		msg := err.Error()
		if s := stderrB.String(); s != "" {
			msg += "\n" + lastLines(s, 3)
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// buildEncodeArgs picks the video encoder based on the user's choice:
// "amd" → AMD GPU (h264_amf, fast), "cpu" → lossless x264, "auto" → AMD if available.
func buildEncodeArgs(encoder string) ([]string, string) {
	if encoder == "amd" || (encoder == "auto" && amfAvailable()) {
		return amdEncodeArgs()
	}
	return cpuEncodeArgs()
}

func cpuEncodeArgs() ([]string, string) {
	// Lossless: CRF 0 keeps the source quality exactly.
	return []string{"-c:v", "libx264", "-crf", "0", "-preset", "fast"}, "CPU无损"
}

func amdEncodeArgs() ([]string, string) {
	// AMD hardware encoder. Quality mode with low QP ≈ visually lossless,
	// several times faster than software x264.
	return []string{
		"-c:v", "h264_amf",
		"-usage", "transcoding",
		"-quality", "quality",
		"-rc", "0", // CQP constant quality
		"-qp_i", "14", "-qp_p", "16", "-qp_b", "16",
	}, "AMD硬件加速"
}

// amfAvailable checks whether ffmpeg has the h264_amf encoder (cached).
func amfAvailable() bool {
	if amfOK != -1 { return amfOK == 1 }
	amfOK = 0
	out, err := exec.Command(ffmpeg_, "-hide_banner", "-encoders").CombinedOutput()
	if err == nil && strings.Contains(string(out), "h264_amf") {
		amfOK = 1
	}
	return amfOK == 1
}

func buildCompressShift(srcLabel, dstLabel string, compress float64, shiftRight bool, shiftPx int) string {
	var chain []string
	prefix := fmt.Sprintf("[%s]", srcLabel)
	if compress > 0 {
		// Squeeze horizontally, keep even width
		prefix += fmt.Sprintf("scale=trunc(iw*(1-%f)/2)*2:ih,", compress)
	} else {
		prefix += ""
	}
	if shiftRight {
		// Shift RIGHT: take left (W-N) pixels, pad left side by N (output = original W)
		// After crop, iw=W-N; pad target = iw+N = original W
		chain = append(chain, fmt.Sprintf("%scrop=iw-%d:ih:0:0,pad=iw+%d:ih:%d:0", prefix, shiftPx, shiftPx, shiftPx))
	} else {
		// Shift LEFT: take right (W-N) pixels, pad right side by N
		chain = append(chain, fmt.Sprintf("%scrop=iw-%d:ih:%d:0,pad=iw+%d:ih:0:0", prefix, shiftPx, shiftPx, shiftPx))
	}
	return strings.Join(chain, ",") + fmt.Sprintf("[%s]", dstLabel)
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
	// ffmpeg -i only: prints metadata to stderr then exits (no full decode).
	cmd := exec.Command(ffmpeg_, "-i", path)
	out, _ := cmd.CombinedOutput()
	w, h := 1920, 1080
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "Stream") || !strings.Contains(line, "Video") {
			continue
		}
		// Find the resolution: a word containing exactly one 'x' where both sides
		// are integers. Trim trailing punctuation (commas, parens).
		for _, word := range strings.Fields(line) {
			word = strings.TrimRight(word, ",)]")
			parts := strings.SplitN(word, "x", 2)
			if len(parts) != 2 { continue }
			ww, e1 := strconv.Atoi(parts[0])
			hh, e2 := strconv.Atoi(parts[1])
			if e1 == nil && e2 == nil && ww >= 16 && hh >= 16 {
				w, h = ww, hh
				return w, h
			}
		}
	}
	return w, h
}

func probeDuration(path string) float64 {
	// ffmpeg -i only: metadata to stderr, exits immediately (no full decode).
	cmd := exec.Command(ffmpeg_, "-i", path)
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

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n { return s }
	return strings.Join(lines[len(lines)-n:], "\n")
}

func abs(x int) int { if x < 0 { return -x }; return x }
