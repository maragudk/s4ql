// Command probe prints SQLite's verdict on each argument, for development only. Not committed.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"maragu.dev/s4ql/internal/sql/oracle"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		s := bufio.NewScanner(os.Stdin)
		s.Buffer(make([]byte, 1<<20), 1<<24)
		for s.Scan() {
			args = append(args, s.Text())
		}
	}
	for _, a := range args {
		a = strings.ReplaceAll(a, `\0`, "\x00")
		v := oracle.Judge(a)
		fmt.Printf("%q: %v %q at %d tail %d %v\n", a, v.Kind, v.Message, v.Offset, v.Tail, v.Reason)
	}
}
