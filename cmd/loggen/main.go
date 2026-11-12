package main

import (
	"fmt"
	"os"

	commitlog "stream-log/internal/log"
)

func main() {
	l, err := commitlog.Open("data/payments-0", commitlog.Config{MaxSegSize: 1 << 20})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer l.Close()

	for i := 0; i < 5; i++ {
		off, err := l.Append([]byte("0712"), []byte(fmt.Sprintf("Paid KES %d", (i+1)*100)))
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("appended offset", off)
	}
}
