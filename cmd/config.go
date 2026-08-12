package main

import "time"

type fileConfig struct {
	APIAddr     string `json:"apiAddr"`
	ConsoleAddr string `json:"consoleAddr"`
	StateFile   string `json:"stateFile"`
	RuntimeDir  string `json:"runtimeDir"`
	Auth        struct {
		Username          string `json:"username"`
		Password          string `json:"password"`
		PasswordHash      string `json:"passwordHash"`
		PasswordFile      string `json:"passwordFile"`
		SessionTTLMinutes int    `json:"sessionTTLMinutes"`
	} `json:"auth"`
}

type options struct {
	APIAddr           string
	ConsoleAddr       string
	StateFile         string
	RuntimeDir        string
	AdminUsername     string
	AdminPassword     string
	AdminPasswordHash string
	SessionTTL        time.Duration
}
