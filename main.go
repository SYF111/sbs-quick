package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
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
	Error    string  `json:"error,omitempty"`
	OutPath  string  `json:"out_path,omitempty"`
	Elapsed  float64 `json:"elapsed"`
}

var (
	jobs    = map[string]*Job{}
	jobsMu  sync.Mutex
	jobSeq  int
	ffmpeg_ string
)

type SingleRequest struct{ Path string; Shift int }
type BatchRequest  struct{ Dir  string; Shift int }

func main() {
	port := "7878"
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--port=") {
		port = strings.TrimPrefix(os.Args[1], "--port=")
	}

	ffmpeg_ = findFFmpeg()

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveUI)
	mux.HandleFunc("/api/single", handleSingle)
	mux.HandleFunc("/api/batch", handleBatch)
	mux.HandleFunc("/api/jobs", handleJobs)
	mux.HandleFunc("/api/stats", handleStats)

	url := fmt.Sprintf("http://127.0.0.1:%s", port)
	log.Printf("SBS Quick — %s", url)
	log.Printf("ffmpeg: %s", ffmpeg_)
	if ffmpeg_ == "" {
		log.Println("WARNING: ffmpeg not found.")
	}
	log.Println("Press Ctrl+C to stop.")
	openBrowser(url)

	if err := http.ListenAndServe("127.0.0.1:"+port, mux); err != nil {
		log.Fatal(err)
	}
}

// findFFmpeg checks if ffmpeg is runnable. Returns "ffmpeg" (name, not full
// path) so Windows can resolve winget aliases that exist in PATH but aren't
// real executables.
func findFFmpeg() string {
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		// Verify it actually runs (winget Links alias passes LookPath but may fail exec)
		if err := exec.Command("ffmpeg", "-version").Run(); err == nil {
			return "ffmpeg"
		}
	}
	home, _ := os.UserHomeDir()
	dirs := []string{}
	switch runtime.GOOS {
	case "windows":
		dirs = []string{
			`C:\ffmpeg\bin`,
			`C:\Program Files\ffmpeg\bin`,
			filepath.Join(os.Getenv("ProgramFiles"), "ffmpeg", "bin"),
			filepath.Join(home, `scoop\apps\ffmpeg\current\bin`),
		}
		// Winget installs to deep package dirs — quick walk to find ffmpeg.exe
		wingetRoot := filepath.Join(home, `AppData\Local\Microsoft\WinGet\Packages`)
		filepath.WalkDir(wingetRoot, func(p string, d os.DirEntry, err error) error {
			if err != nil { return filepath.SkipDir }
			if d.IsDir() {
				if depth(p) > 5 { return filepath.SkipDir }
				return nil
			}
			if strings.EqualFold(d.Name(), "ffmpeg.exe") {
				dirs = append(dirs, filepath.Dir(p))
			}
			return nil
		})
	case "darwin":
		dirs = []string{"/opt/homebrew/bin", "/usr/local/bin", filepath.Join(home, ".local/bin")}
	default:
		dirs = []string{"/usr/bin", "/usr/local/bin", filepath.Join(home, ".local/bin")}
	}
	for _, d := range dirs {
		p := filepath.Join(d, "ffmpeg"+ext())
		if _, err := os.Stat(p); err == nil { return p }
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
	default:        c = exec.Command("xdg-open", url)
	}
	c.Start()
}

func serveUI(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.ParseFS(webUI, "index.html")
	tmpl.Execute(w, nil)
}

func handleSingle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { http.Error(w, "POST only", 405); return }
	var req SingleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, err.Error(), 400); return }
	if req.Path == "" { http.Error(w, "missing path", 400); return }
	if req.Shift < 1 { req.Shift = 6 }
	job := newJob(filepath.Base(req.Path))
	go runSingle(job, req.Path, req.Shift)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

func handleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { http.Error(w, "POST only", 405); return }
	var req BatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, err.Error(), 400); return }
	if req.Dir == "" { http.Error(w, "missing dir", 400); return }
	if req.Shift < 1 { req.Shift = 6 }
	go runBatch(req.Dir, req.Shift)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	jobsMu.Lock(); defer jobsMu.Unlock()
	list := make([]*Job, 0, len(jobs))
	for _, j := range jobs { list = append(list, j) }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ffmpeg": ffmpeg_ != "", "os": runtime.GOOS, "arch": runtime.GOARCH,
	})
}

func newJob(filename string) *Job {
	jobsMu.Lock(); defer jobsMu.Unlock()
	jobSeq++
	j := &Job{ID: strconv.Itoa(jobSeq), Filename: filename, Status: "queued"}
	jobs[j.ID] = j
	return j
}

func updateJob(id, status string, progress float64, errMsg, outPath string) {
	jobsMu.Lock(); defer jobsMu.Unlock()
	if j, ok := jobs[id]; ok {
		j.Status = status; j.Progress = progress; j.Error = errMsg; j.OutPath = outPath
	}
}

func runSingle(job *Job, inPath string, shift int) {
	start := time.Now()
	updateJob(job.ID, "running", 0, "", "")

	if ffmpeg_ == "" {
		updateJob(job.ID, "error", 0, "ffmpeg not found. Install: brew install ffmpeg / winget install ffmpeg", "")
		return
	}

	// Check input exists
	if !fileExists(inPath) {
		updateJob(job.ID, "error", 0, "File not found: "+inPath, "")
		return
	}

	outDir := filepath.Dir(inPath)
	base := strings.TrimSuffix(filepath.Base(inPath), filepath.Ext(inPath))
	outPath := filepath.Join(outDir, base+"_SBS.mp4")
	for n := 1; fileExists(outPath); n++ {
		outPath = filepath.Join(outDir, fmt.Sprintf("%s_SBS_%d.mp4", base, n))
	}

	updateJob(job.ID, "running", 0.05, "", "")

	dur := getDuration(inPath)
	P := shift
	filter := fmt.Sprintf(
		"[0:v]split[l][r];"+
			"[l]crop=iw-%d:ih:0:0,pad=iw:ih:%d:0[lp];"+
			"[r]crop=iw-%d:ih:%d:0,pad=iw:ih:0:0[rp];"+
			"[lp][rp]hstack[out]",
		P, P, P, P)

	args := []string{
		"-y", "-i", inPath,
		"-filter_complex", filter,
		"-map", "[out]", "-map", "0:a?",
		"-c:v", "libx264", "-crf", "18", "-preset", "fast",
		"-pix_fmt", "yuv420p", "-c:a", "aac",
		"-progress", "pipe:1", "-nostats",
		outPath,
	}
	log.Printf("[%s] ffmpeg %s", job.ID, strings.Join(args, " "))

	cmd := exec.Command(ffmpeg_, args...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		updateJob(job.ID, "error", 0, err.Error(), "")
		return
	}

	// Drain stderr in background
	stderrB := new(strings.Builder)
	go func() { io.Copy(stderrB, stderr) }()

	// Parse progress
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
						updateJob(job.ID, "running", p, "", "")
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
	updateJob(job.ID, "done", 1.0, "", outPath)
	log.Printf("[%s] DONE: %s (%.1fs)", job.ID, outPath, elapsed)
}

func runBatch(dir string, shift int) {
	if ffmpeg_ == "" {
		j := newJob("batch"); updateJob(j.ID, "error", 0, "ffmpeg not found", ""); return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		j := newJob("batch"); updateJob(j.ID, "error", 0, err.Error(), ""); return
	}
	exts := map[string]bool{
		".mp4": true, ".mkv": true, ".mov": true, ".avi": true,
		".flv": true, ".webm": true, ".m4v": true, ".ts": true,
		".wmv": true, ".mpg": true, ".mpeg": true,
	}
	var videos []string
	for _, e := range entries {
		if e.IsDir() { continue }
		if exts[strings.ToLower(filepath.Ext(e.Name()))] {
			videos = append(videos, filepath.Join(dir, e.Name()))
		}
	}
	log.Printf("Batch: %d videos in %s", len(videos), dir)
	for _, v := range videos {
		job := newJob(filepath.Base(v))
		runSingle(job, v, shift)
	}
}

func getDuration(path string) float64 {
	if ffmpeg_ == "" { return 60 }
	ffprobe := strings.Replace(ffmpeg_, "ffmpeg", "ffprobe", 1)
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil { return 60 }
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d
}

func parseTime(ts string) float64 {
	var h, m int; var s float64
	fmt.Sscanf(ts, "%d:%d:%f", &h, &m, &s)
	return float64(h*3600+m*60) + s
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func depth(p string) int {
	// relative to home dir
	home, _ := os.UserHomeDir()
	rel := strings.TrimPrefix(p, home)
	return len(strings.Split(rel, string(os.PathSeparator)))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n { return s }
	return strings.Join(lines[len(lines)-n:], "\n")
}
