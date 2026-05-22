package main

import (
	"fmt"
	"tc-connect/config"
)

func initConfigPath(flagValue string) {
	config.ConfigPath = resolveConfigPath(flagValue)
	fmt.Printf("config file path: %s\n", config.ConfigPath)
}
