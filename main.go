package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var clients = make(map[string]*websocket.Conn)
var mu sync.Mutex

type BuildRequest struct {
	RepoUrl string `json:"repoUrl"`
}

type BuildResponse struct {
	BuildId  string `json:"buildId"`
	CommitID string `json:"commitId"`
}

type LogStreamer struct {
	buildId string
}

func (ls *LogStreamer) Write(p []byte) (n int, err error) {
	mu.Lock()
	defer mu.Unlock()
	if conn, ok := clients[ls.buildId]; ok {
		conn.WriteMessage(websocket.TextMessage, p)
	}
	return len(p), nil
}

var db *sql.DB

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "./builds.db")
	if err != nil {
		log.Fatal(err)
	}
	createTable := `
    CREATE TABLE IF NOT EXISTS builds (
        id TEXT PRIMARY KEY,
        repo_url TEXT,
        commit_id TEXT,
        timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
    );
    `
	_, err = db.Exec(createTable)
	if err != nil {
		log.Fatal(err)
	}
}

func saveBuild(buildID, repoURL, commitID string) error {
	_, err := db.Exec("INSERT INTO builds (id, repo_url, commit_id) VALUES (?, ?, ?)", buildID, repoURL, commitID)
	return err
}

func getLastBuild() (BuildResponse, error) {

	var build BuildResponse
	row := db.QueryRow("SELECT id, commit_id FROM builds ORDER BY timestamp DESC LIMIT 1")
	err := row.Scan(&build.BuildId, &build.CommitID)
	return build, err
}

func buildHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var req BuildRequest
	json.NewDecoder(r.Body).Decode(&req)

	buildId := uuid.New().String()

	var commitID string

	go func() {
		// Clone the repository
		repoDir := fmt.Sprintf("/tmp/%s", buildId)
		cmd := exec.Command("git", "clone", req.RepoUrl, repoDir)
		cmd.Stdout = &LogStreamer{buildId: buildId}
		cmd.Stderr = &LogStreamer{buildId: buildId}
		if err := cmd.Run(); err != nil {
			log.Printf("Error cloning repository: %v", err)
			return
		}

		// Get the latest commit ID
		cmd = exec.Command("git", "-C", repoDir, "rev-parse", "HEAD")
		commitIDBytes, err := cmd.Output()
		if err != nil {
			log.Printf("Error getting latest commit ID: %v", err)
			return
		}
		commitID = strings.TrimSpace(string(commitIDBytes))

		// Build the Docker image using Buildx
		imageName := fmt.Sprintf("myapp:%s", commitID)
		cmd = exec.Command("docker", "buildx", "build", repoDir, "--tag", imageName, "--output=type=docker")
		cmd.Stdout = &LogStreamer{buildId: buildId}
		cmd.Stderr = &LogStreamer{buildId: buildId}
		if err := cmd.Run(); err != nil {
			log.Printf("Error building Docker image: %v", err)
			return
		}

		// Notify frontend that the build process is complete
		mu.Lock()
		if conn, ok := clients[buildId]; ok {
			conn.WriteMessage(websocket.TextMessage, []byte("BUILD_COMPLETE"))
			conn.Close()
			delete(clients, buildId)
		}
		mu.Unlock()

		// Save build details to the database
		if err := saveBuild(buildId, req.RepoUrl, commitID); err != nil {
			log.Printf("Error saving build details: %v", err)
		}

		// Clean up: delete the repository directory
		if err := os.RemoveAll(repoDir); err != nil {
			log.Printf("Error removing repository directory: %v", err)
		}
	}()

	resp := BuildResponse{BuildId: buildId, CommitID: commitID}
	json.NewEncoder(w).Encode(resp)
}

func lastBuildHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	build, err := getLastBuild()
	if err != nil {
		http.Error(w, "Could not get last build details", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(build)
}

func logsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	buildId := vars["buildId"]

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "Could not open websocket connection", http.StatusBadRequest)
		return
	}

	mu.Lock()
	clients[buildId] = conn
	mu.Unlock()
}
func CorsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println("executing middleware", r.Method)

		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Origin, Content-type, Authorization")
			w.Header().Set("Content-type", "application/json")
			return
		}
		next.ServeHTTP(w, r)
		log.Println("Executing middleware again")
	})
}

func main() {
	initDB()
	defer db.Close()
	r := mux.NewRouter()
	r.HandleFunc("/api/build", buildHandler).Methods("POST")
	r.HandleFunc("/api/last-build", lastBuildHandler).Methods("GET")
	r.HandleFunc("/api/logs/{buildId}", logsHandler)

	http.Handle("/", r)
	fmt.Println("Running server on 8080 ")
	log.Fatal(http.ListenAndServe(":8080", CorsMiddleware(r)))
}
