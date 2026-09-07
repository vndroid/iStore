package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/kane/istore/internal/imageinfo"
)

func main() {
	for _, p := range os.Args[1:] {
		f, err := os.Open(p)
		if err != nil {
			fmt.Println(p, err)
			continue
		}
		st, _ := f.Stat()
		info, err := imageinfo.Read(f, st.Size())
		f.Close()
		if err != nil {
			fmt.Printf("%-16s %v\n", p, err)
			continue
		}
		b, _ := json.Marshal(info)
		fmt.Printf("%-16s %s\n", p, string(b))
	}
}
