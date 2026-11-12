package library

import (
	"math/rand"
	"sync"
	"time"
)

// Tests are not library code, so nothing here is reported.
func testOnly() {
	go func() {}()
	time.Sleep(time.Second)
	var wg sync.WaitGroup
	wg.Wait()
	_ = rand.Int()
}
