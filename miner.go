package main

import (
	"os/signal"
	"fmt"
	"bytes"
	"log"
	"os/exec"
	"os"
	"time"
	"io/ioutil"
	"crypto/sha1"
	"encoding/hex"
	"net"
	"sync"
	"errors"

	"github.com/garyburd/redigo/redis"
)

const UNAME = "Watermelon"
const CMD_HASH = "./sha1"
const NONCELEN = 16
var sigint = make(chan os.Signal, 1)

// Store a global of the successful commits we've done, so that
// we can verify incoming messages from redis (I think there may
// be race conditions there? Makes things easier anyway.)
var hashes = struct{
	mu sync.Mutex
	set map[string]struct{}
}{
	set: make(map[string]struct{}),
}

func store_hash(hash string) {
	hashes.mu.Lock()
	hashes.set[hash] = struct{}{}
	hashes.mu.Unlock()
}
func seen_hash(hash string) bool {
	hashes.mu.Lock()
	_, ok := hashes.set[hash]
	hashes.mu.Unlock()
	return ok
}


type WorkUnit struct {
	// Should be a hex string.
	difficulty string

	// Header used so that we can chop it off later
	header string

	// Output will be filled out before being sent
	// back on the channel
	tononce, nonced []byte

	// Hash of the nonced
	hash string
}

// The miner goroutine receives on a channel of WorkUnits, and will
// attempt to solve them using the CMD_HASH function. Upon receiving
// a new piece of work, it throws out its old work and starts again.
// Any work it completes it returns with the same pointer as was
// passed in.
func miner(recv, send chan *WorkUnit) {
	worku := <-recv
	for {
		// Prepare the call to the miner
		var out, serr bytes.Buffer
		cmd := exec.Command(CMD_HASH, worku.difficulty)
		cmd.Stdin = bytes.NewBuffer(worku.tononce)
		cmd.Stdout = &out
		cmd.Stderr = &serr

		// Start the miner
		done := make(chan error, 1);
		begin := time.Now()
		log.Println("Mining...")
		go func() {
			cmd.Start()
			done2 := make(chan error, 1)
			go func() {
				done2 <- cmd.Wait()
			}()

			select {
			case err := <- done2:
				done <- err
			case <- sigint:
				log.Println("Got SIGINT, killing hasher")
				cmd.Process.Signal(os.Interrupt)
				cmd.Wait()
				done <- errors.New("SIGINT")
			}
		}()

		// This is messy...
		waitfornew := true

		// Wait for it to finish, or for us to get a new WorkItem
		select {
		case err := <-done:
			end := time.Now()
			solve_time := end.Sub(begin)
			if err != nil {
				fmt.Println("Stdout:")
				fmt.Println(string(out.Bytes()))
				fmt.Println("Stderr:")
				fmt.Println(string(serr.Bytes()))
				log.Fatalln(CMD_HASH, "exited with nonzero status:", err)
			}
			worku.nonced = out.Bytes()
			send <- worku
			log.Println("Solved work unit in", solve_time)

		case newu := <-recv:
			// Kill the process nicely.
			log.Println("Recieved new work, killing process.")
			cmd.Process.Signal(os.Interrupt)
			cmd.Wait()
			log.Println("Process killed")

			worku = newu
			waitfornew = false
		}

		if waitfornew {
			worku = <-recv
		}
	}
}

// Build a new commit
func get_workunit(message string) *WorkUnit {
	tree, err := exec.Command("git", "write-tree").Output()
	if err != nil {
		log.Fatalln("Could not execute git write-tree:", err)
	}
	tree = bytes.TrimSpace(tree)

	parent, err := ioutil.ReadFile(".git/refs/heads/master")
	if err != nil {
		log.Fatalln("Could not read .git/refs/heads/master:", err)
	}
	parent = bytes.TrimSpace(parent)

	diff, err := ioutil.ReadFile("difficulty.txt")
	if err != nil {
		log.Fatalln("Could not read difficulty.txt:", err)
	}
	diff = bytes.TrimSpace(diff)

	tstamp := time.Now().Unix()

	commit := fmt.Sprintf(`tree %s
parent %s
author CTF user <me@example.com> %d +1000
%d +1

%s

`, string(tree), string(parent), tstamp, tstamp, message)
	
	// NONCELEN chars will be added by the hasher
	header := fmt.Sprintf("commit %d\x00", len(commit) + NONCELEN)
	for (len(header) + len(commit)) % 64 != 39 {
		commit += " "
		header = fmt.Sprintf("commit %d\x00", len(commit) + NONCELEN)
	}

	return &WorkUnit{
		difficulty: string(diff),
		header: header,
		tononce: []byte(header + commit),
		nonced: nil,
	}
}

// Git fetch, hard reset
func fetch_and_reset() error {
retry_fetch:
	cmd := exec.Command("git", "fetch", "origin", "master")
	ret := make(chan error, 1)
	go func() {
		cmd.Start()
		ret <- cmd.Wait()
	}()
	select {
	case err := <-ret:
		if err != nil {
			return err
		}
	case <-time.After(time.Minute * 1):
		fmt.Println("Fetch timed out, killing ssh persistent conn")
		err := exec.Command("ssh", "-O", "exit", "gitcoin").Run()
		if err != nil {
			fmt.Println("ssh -O exit gitcoin returned error:", err)
		}
		goto retry_fetch
	}

	return exec.Command("git", "reset", "--hard", "origin/master").Run()
}
// Bump the ledger
func bump_ledger() {
	if err := exec.Command("python", "bumpledger.py", UNAME).Run(); err != nil {
		log.Fatalln("Could not bump ledger:", err)
	}
	if err := exec.Command("git", "add", "LEDGER.txt").Run(); err != nil {
		log.Fatalln("Could not git add ledger:", err)
	}
}

// Check and commit a completed work object.
// Returns true on push success, false on push fail, and
// aborts if something else goes wrong.
func check_and_complete(unit *WorkUnit) bool {
	hash_bytes := sha1.Sum(unit.nonced)
	hash_hex := hex.EncodeToString(hash_bytes[:])
	unit.hash = hash_hex
	if hash_hex >= unit.difficulty {
		log.Println("Hash of", hash_hex, "not small enough for difficulty", unit.difficulty);
		log.Println("Work unit:", string(unit.tononce))
		return false
	}
	
	// Store the hash so we know not to get confused about it in the future.
	store_hash(hash_hex)

	commit := unit.nonced[len(unit.header):]
	if err := ioutil.WriteFile("commit.txt", commit, 0644); err != nil {
		log.Fatalln("Error writing to commit.txt:", err)
	}

	if err := exec.Command("git", "hash-object", "-t", "commit", "commit.txt", "-w").Run(); err != nil {
		log.Fatalln("Git hash-object error:", err)
	}

	if err := exec.Command("git", "reset", "--hard", hash_hex).Run(); err != nil {
		log.Fatalln("Git reset to", hash_hex, "error:", err)
	}

	cmd := exec.Command("git", "push", "origin", "master")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Println("Git push failed:", err)
		return false
	}
	return true
}

// Set up something watching the redis stream to tell us when to update early.
func redis_watch(send chan string) {
	addr, err := net.ResolveTCPAddr("tcp", "elec5616.com:6379")
	if err != nil {
		log.Println("Redis stopping: Could not resolve hostname:", err)
		return
	}
	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		log.Println("Redis stopping: Could not dial redis server:", err)
		return
	}
	
	// Magic socket options from redis/src/anet.c. We'll see if we
	// can get away with not all of them
	if err := conn.SetKeepAlive(true); err != nil {
		log.Println("Redis stopping: Could not set the keep alive socket option:", err)
		return
	}

	if err := conn.SetKeepAlivePeriod(time.Second * 15); err != nil {
		log.Println("Redis stopping: Could not set the keep idle socket option:", err)
		return
	}

	c := redis.NewConn(conn, 0, 0)
	defer c.Close()

	psc := redis.PubSubConn{Conn: c}
	if err := psc.Subscribe("gitcoin"); err != nil {
		log.Println("Could not subscribe on redis:", err)
	}
	for {
		reply := psc.Receive()
		switch v := reply.(type) {
		case redis.Subscription:
			log.Println("Redis Listening to channel:", v.Channel)
		case redis.Message:
			fields := bytes.Fields(v.Data)
			if len(fields) != 2 {
				fmt.Println("Strange message on redis:", string(v.Data))
				continue
			}
			hash := string(fields[0])
			if !seen_hash(hash) {
				log.Println("Redis: unseen hash", hash)
				send <- hash
			} else {
				log.Println("Redis: ignoring hash", hash)
			}
		case error:
			log.Println("Stopping redis: Error in switch:", v)
			return
		}
	}
}

func main() {
	// Want to nicely exit the cuda thing
	signal.Notify(sigint, os.Interrupt)

	// Start our redis watcher
	update := make(chan string, 10)
	go redis_watch(update)

	// Start our miner
	send, recv := make(chan *WorkUnit, 10), make(chan *WorkUnit, 10)
	go miner(send, recv)

	// We we successful just then? (no need to fetch)
	succ := false
	for {
		message := "u wot m8"
		if !succ {
			log.Println("Performing a fetch and reset")
			if err := fetch_and_reset(); err != nil {
				// Perhaps a transient network error
				log.Println("Fetch and reset errored:", err)
				log.Println("Sleeping for a while...")
				time.Sleep(time.Minute * 1)
				continue
			}
		} else {
			log.Println("Skipping fetch...")
		}
		bump_ledger()

		unit := get_workunit(message)
		log.Println("Sending work unit to miner")
		send <- unit
	retry_recv:
		select {
		case runit := <- recv:
			// An attempt to avoid the race condition here.
			if runit != unit {
				log.Println("Received stale work unit from channel, retrying")
				goto retry_recv
			} else {
				log.Println("Received current work unit from channel")
			}
		case hash := <- update:
			log.Println("Received new hash", hash, "from redis, restarting")
			succ = false
			continue
		}
			

		log.Println("Attempting to commit...")
		succ = check_and_complete(unit)
		if succ {
			log.Println("SUCCESS! Mined a bitcoin")
		} else {
			log.Println("Fail :'( Someone got there before us")
		}
	}
}

