package main

import (
	"encoding/json"
	"net/http"
)

// Placeholder data structures
var rateLimits = map[string]int{
	"user1": 100,
	"user2": 50,
}
var penaltyQueue = []string{"user2", "user3"}
var requestLogs = []string{"user1: GET /api", "user2: POST /api"}

func main() {
	http.HandleFunc("/rate-limits", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(rateLimits)
	})

	http.HandleFunc("/penalty-queue", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(penaltyQueue)
	})

	http.HandleFunc("/request-logs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(requestLogs)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<h1>SketchGate Dashboard</h1>
<ul>
<li><a href='/rate-limits'>Rate Limits</a></li>
<li><a href='/penalty-queue'>Penalty Queue</a></li>
<li><a href='/request-logs'>Request Logs</a></li>
</ul>`))
	})

	       println("SketchGate UI server started on http://localhost:8080")
	       err := http.ListenAndServe(":8080", nil)
	       if err != nil {
		       println("Error starting server:", err.Error())
	       }
}
