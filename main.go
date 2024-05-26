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
	_ "github.com/lib/pq"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var clients = make(map[string]*websocket.Conn)
var mu sync.Mutex

type Project struct {
	ID         int    `json:"id"`
	RepoURL    string `json:"repoUrl"`
	Token      string `json:"token"`
	Autodeploy bool   `json:"autodeploy"`
	Branch     string `json:"branch"`
}

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
	connStr := "postgres://postgres:postgres@localhost/mydb?sslmode=disable"
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal(err)
	}
	createTable := `
    CREATE TABLE IF NOT EXISTS builds (
        id TEXT PRIMARY KEY,
        repo_url TEXT,
        commit_id TEXT,
        timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );
    `
	_, err = db.Exec(createTable)
	if err != nil {
		log.Fatal(err)
	}

	createProjectTable := `
    CREATE TABLE IF NOT EXISTS projects (
        id SERIAL PRIMARY KEY,
        repo_url TEXT,
        token TEXT,
        autodeploy BOOLEAN,
        branch TEXT
    );
    `
	_, err = db.Exec(createProjectTable)
	if err != nil {
		log.Fatal(err)
	}
}

func saveBuild(buildID, repoURL, commitID string) error {
	_, err := db.Exec("INSERT INTO builds (id, repo_url, commit_id) VALUES ($1, $2, $3)", buildID, repoURL, commitID)
	return err
}

func getLastBuild() (BuildResponse, error) {
	var build BuildResponse
	row := db.QueryRow("SELECT id, commit_id FROM builds ORDER BY timestamp DESC LIMIT 1")
	err := row.Scan(&build.BuildId, &build.CommitID)
	return build, err
}

func saveProject(repoURL, token string, autodeploy bool, branch string) error {
	_, err := db.Exec("INSERT INTO projects (repo_url, token, autodeploy, branch) VALUES ($1, $2, $3, $4)", repoURL, token, autodeploy, branch)
	return err
}

func getCurrentProject() (Project, error) {
	var project Project
	row := db.QueryRow("SELECT id, repo_url, token, autodeploy, branch FROM projects ORDER BY id DESC ")
	err := row.Scan(&project.ID, &project.RepoURL, &project.Token, &project.Autodeploy, &project.Branch)
	return project, err
}

func buildHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var req BuildRequest
	json.NewDecoder(r.Body).Decode(&req)

	buildId := uuid.New().String()
	var commitID string

	// Start the build process in a separate goroutine
	go func() {
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

		// Save build details to the database
		if err := saveBuild(buildId, req.RepoUrl, commitID); err != nil {
			log.Printf("Error saving build details: %v", err)
		}

		// Clean up: delete the repository directory
		if err := os.RemoveAll(repoDir); err != nil {
			log.Printf("Error removing repository directory: %v", err)
		}

		// Notify frontend that the build process is complete
		mu.Lock()
		if conn, ok := clients[buildId]; ok {
			conn.WriteMessage(websocket.TextMessage, []byte("BUILD_COMPLETE"))
			conn.Close()
			delete(clients, buildId)
		}
		mu.Unlock()

	}()
	resp := BuildResponse{BuildId: buildId}
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

func projectHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var req struct {
		RepoURL    string `json:"repoUrl"`
		Token      string `json:"token"`
		Autodeploy bool   `json:"autodeploy"`
		Branch     string `json:"branch,omitempty"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	err := saveProject(req.RepoURL, req.Token, req.Autodeploy, req.Branch)
	if err != nil {
		http.Error(w, "Could not save project details", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
func currentProjectHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	project, err := getCurrentProject()
	if err != nil {
		http.Error(w, "Could not get current project details", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(project)
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

func deployHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	var req struct {
		RepoURL  string `json:"repoUrl"`
		CommitID string `json:"commitId"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	// Use the commit ID as the image tag
	imageName := fmt.Sprintf("myapp:%s", req.CommitID)
	fmt.Println("image tag: ", imageName)
	// Run the Docker container using the built image
	cmd := exec.Command("docker", "run", "-d", "--name", "testcontainer", imageName)
	err := cmd.Run()
	if err != nil {
		log.Printf("Error running Docker container: %v", err)
		http.Error(w, "Error running Docker container", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
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
	r.HandleFunc("/api/project", projectHandler).Methods("POST")
	r.HandleFunc("/api/current-project", currentProjectHandler).Methods("GET")
	r.HandleFunc("/api/deploy", deployHandler).Methods("POST")
	r.HandleFunc("/api/logs/{buildId}", logsHandler)

	http.Handle("/", r)
	fmt.Println("Running webserver on 8080: ")
	log.Fatal(http.ListenAndServe(":8080", CorsMiddleware(r)))
}
