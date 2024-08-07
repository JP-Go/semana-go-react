package main

import (
	"bytes"
	"fmt"
	"os/exec"

	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load("../.env"); err != nil {
		panic(err)
	}

	cmd := exec.Command(
		"tern",
		"migrate",
		"--migrations",
		"./internal/store/pgstore/migrations",
		"--config",
		"./internal/store/pgstore/migrations/tern.conf",
	)
	buf := bytes.Buffer{}
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		fmt.Println(buf.String())
		panic(err)
	}
}
