package main

// Railway variables: REPLICATE_API_TOKEN, REPLICATE_MODEL, APP_PASSWORD
// Optional: CLIPS (default 4), IMAGE_FIELD, LIPSYNC_MODEL, LIPSYNC_VIDEO_FIELD, LIPSYNC_AUDIO_FIELD, TTS_VOICE
// Model naam: official ho to owner/name, community ho to owner/name:versionhash

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Job struct{ Status, Progress, Error string }

var (
	jobs = map[string]*Job{}
	mu   sync.Mutex
)

func setJob(id, status, progress, errMsg string) {
	mu.Lock()
	jobs[id] = &Job{status, progress, errMsg}
	mu.Unlock()
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func authed(r *http.Request) bool {
	pw := os.Getenv("APP_PASSWORD")
	return pw == "" || r.Header.Get("X-Password") == pw || r.URL.Query().Get("p") == pw
}

func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Password")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == "OPTIONS" {
			return
		}
		if !authed(r) {
			http.Error(w, `{"error":"wrong password"}`, 401)
			return
		}
		next(w, r)
	}
}

func replicate(method, url string, body []byte) (map[string]any, error) {
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+os.Getenv("REPLICATE_API_TOKEN"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(data, &out)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("replicate %d: %v", resp.StatusCode, out["detail"])
	}
	return out, nil
}

// runModel: Replicate model chalata hai, output URL return karta hai
func runModel(model string, input map[string]any) (string, error) {
	endpoint := "https://api.replicate.com/v1/models/" + model + "/predictions"
	payload := map[string]any{"input": input}
	if i := strings.Index(model, ":"); i >= 0 { // community model: owner/name:version
		endpoint = "https://api.replicate.com/v1/predictions"
		payload["version"] = model[i+1:]
	}
	body, _ := json.Marshal(payload)
	out, err := replicate("POST", endpoint, body)
	if err != nil {
		return "", err
	}
	id, _ := out["id"].(string)
	for i := 0; i < 150; i++ { // max ~10 minute
		time.Sleep(4 * time.Second)
		p, err := replicate("GET", "https://api.replicate.com/v1/predictions/"+id, nil)
		if err != nil {
			return "", err
		}
		switch p["status"] {
		case "succeeded":
			switch o := p["output"].(type) {
			case string:
				return o, nil
			case []any:
				if len(o) > 0 {
					s, _ := o[0].(string)
					return s, nil
				}
			}
			return "", errors.New("output nahi mila")
		case "failed", "canceled":
			return "", fmt.Errorf("model fail: %v", p["error"])
		}
	}
	return "", errors.New("timeout")
}

func makeClip(prompt, imageURI string) (string, error) {
	input := map[string]any{"prompt": prompt}
	if imageURI != "" {
		input[env("IMAGE_FIELD", "image")] = imageURI
	}
	return runModel(os.Getenv("REPLICATE_MODEL"), input)
}

// upload: file Replicate par chadha kar URL deta hai
func upload(path string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("content", filepath.Base(path))
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	io.Copy(fw, f)
	f.Close()
	mw.Close()
	req, _ := http.NewRequest("POST", "https://api.replicate.com/v1/files", &buf)
	req.Header.Set("Authorization", "Bearer "+os.Getenv("REPLICATE_API_TOKEN"))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	urls, _ := out["urls"].(map[string]any)
	u, _ := urls["get"].(string)
	if u == "" {
		return "", errors.New("upload fail")
	}
	return u, nil
}

func duration(path string) string {
	out, _ := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	return strings.TrimSpace(string(out))
}

func download(url, path string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func ffmpeg(args ...string) error {
	out, err := exec.Command("ffmpeg", append([]string{"-y"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg: %s", string(out[max(0, len(out)-300):]))
	}
	return nil
}

func toDataURI(path string) string {
	b, _ := os.ReadFile(path)
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(b)
}

func run(id, prompt, image, speech string) {
	n, _ := strconv.Atoi(env("CLIPS", "4"))
	dir := filepath.Join("/tmp", id)
	os.MkdirAll(dir, 0755)
	var list strings.Builder
	current := image

	for i := 0; i < n; i++ {
		setJob(id, "processing", fmt.Sprintf("Clip %d/%d ban rahi hai", i+1, n), "")
		p := prompt
		if i > 0 {
			p += ". Continue the same scene, same character, same face and clothes."
		}
		url, err := makeClip(p, current)
		if err != nil {
			setJob(id, "failed", "", err.Error())
			return
		}
		clip := filepath.Join(dir, fmt.Sprintf("clip_%d.mp4", i))
		if err := download(url, clip); err != nil {
			setJob(id, "failed", "", err.Error())
			return
		}
		list.WriteString(fmt.Sprintf("file '%s'\n", clip))

		// aakhri frame -> agle clip ka starting image
		last := filepath.Join(dir, fmt.Sprintf("last_%d.jpg", i))
		if err := ffmpeg("-sseof", "-0.2", "-i", clip, "-update", "1", "-frames:v", "1", last); err != nil {
			setJob(id, "failed", "", err.Error())
			return
		}
		current = toDataURI(last)
	}

	setJob(id, "processing", "Clips jor raha hu...", "")
	os.WriteFile(filepath.Join(dir, "list.txt"), []byte(list.String()), 0644)
	joined := filepath.Join(dir, "joined.mp4")
	if err := ffmpeg("-f", "concat", "-safe", "0", "-i", filepath.Join(dir, "list.txt"),
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-an", joined); err != nil {
		setJob(id, "failed", "", err.Error())
		return
	}
	final := filepath.Join(dir, "final.mp4")
	if speech == "" {
		os.Rename(joined, final)
	} else {
		setJob(id, "processing", "Urdu awaaz bana raha hu...", "")
		voice := filepath.Join(dir, "voice.mp3")
		if out, err := exec.Command("edge-tts", "--voice", env("TTS_VOICE", "ur-PK-UzmaNeural"),
			"--text", speech, "--write-media", voice).CombinedOutput(); err != nil {
			setJob(id, "failed", "", "TTS fail: "+string(out))
			return
		}
		// awaaz ko video ki length tak khamoshi se bhar do
		padded := filepath.Join(dir, "voice_pad.mp3")
		if err := ffmpeg("-i", voice, "-af", "apad", "-t", duration(joined), padded); err != nil {
			setJob(id, "failed", "", err.Error())
			return
		}
		lipModel := os.Getenv("LIPSYNC_MODEL")
		if lipModel == "" {
			// lip-sync model nahi laga: awaaz bas upar se lag jayegi
			if err := ffmpeg("-i", joined, "-i", padded, "-map", "0:v", "-map", "1:a",
				"-c:v", "copy", "-c:a", "aac", "-shortest", final); err != nil {
				setJob(id, "failed", "", err.Error())
				return
			}
		} else {
			setJob(id, "processing", "Honth awaaz ke sath mila raha hu...", "")
			vURL, err1 := upload(joined)
			aURL, err2 := upload(padded)
			if err1 != nil || err2 != nil {
				setJob(id, "failed", "", "upload fail")
				return
			}
			out, err := runModel(lipModel, map[string]any{
				env("LIPSYNC_VIDEO_FIELD", "video"): vURL,
				env("LIPSYNC_AUDIO_FIELD", "audio"): aURL,
			})
			if err == nil {
				err = download(out, final)
			}
			if err != nil {
				setJob(id, "failed", "", err.Error())
				return
			}
		}
	}
	setJob(id, "succeeded", "Video ready", "")
}

func generate(w http.ResponseWriter, r *http.Request) {
	var in struct{ Prompt, Image, Speech string }
	json.NewDecoder(r.Body).Decode(&in)
	if in.Prompt == "" {
		http.Error(w, `{"error":"prompt required"}`, 400)
		return
	}
	b := make([]byte, 6)
	rand.Read(b)
	id := hex.EncodeToString(b)
	setJob(id, "processing", "Start ho raha hai", "")
	go run(id, in.Prompt, in.Image, in.Speech)
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func status(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	j := jobs[r.URL.Query().Get("id")]
	mu.Unlock()
	if j == nil {
		http.Error(w, `{"error":"not found"}`, 404)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": j.Status, "progress": j.Progress, "error": j.Error})
}

func video(w http.ResponseWriter, r *http.Request) {
	id := filepath.Base(r.URL.Query().Get("id"))
	http.ServeFile(w, r, filepath.Join("/tmp", id, "final.mp4"))
}

func main() {
	http.HandleFunc("/generate", cors(generate))
	http.HandleFunc("/status", cors(status))
	http.HandleFunc("/video", cors(video))
	http.ListenAndServe(":"+env("PORT", "8080"), nil)
}
