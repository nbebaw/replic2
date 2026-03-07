package handler

// HealthResponse is the JSON body returned by GET /healthz.
type HealthResponse struct {
	Status   string `json:"status"`
	Uptime   string `json:"uptime"`
	Hostname string `json:"hostname"`
}

// HelloResponse is the JSON body returned by GET /.
type HelloResponse struct {
	Message   string `json:"message"`
	App       string `json:"app"`
	Version   string `json:"version"`
	Hostname  string `json:"hostname"`
	Namespace string `json:"namespace"`
	Timestamp string `json:"timestamp"`
}

type Response struct {
	Name        string `json:"name"`
	Phase       string `json:"phase"`
	CompletedAt string `json:"completedAt"`
}
