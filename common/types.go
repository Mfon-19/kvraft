package common

type ClientRequest struct {
	Type  string
	Key   string
	Value string
}

type ClientResponse struct {
	Success bool
	Value   string
	Error   string
	Leader  string
}
