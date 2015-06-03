package main

import (
	"log"
	"reflect"
	"net"
	"time"

	"github.com/garyburd/redigo/redis"
)


func main() {
	addr, err := net.ResolveTCPAddr("tcp", "elec5616.com:6379")
	if err != nil {
		panic(err)
	}
	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		panic(err)
	}
	if err := conn.SetKeepAlive(true); err != nil {
		panic(err)
	}
	if err := conn.SetKeepAlivePeriod(time.Second * 15); err != nil {
		panic(err)
	}
	c := redis.NewConn(conn, 0, 0)
	defer c.Close()

	psc := redis.PubSubConn{Conn: c}
	channels := []string{"gitcoin"}
	for _, c := range channels {
		if err := psc.Subscribe(c); err != nil {
			panic(err)
		}
	}
	for {
		reply := psc.Receive()
		switch v := reply.(type) {
		case redis.Message:
			log.Printf("%s: message: %s\n", v.Channel, v.Data)
		case redis.Subscription:
			log.Printf("%s: %s %d\n", v.Channel, v.Kind, v.Count)
		case redis.PMessage:
			log.Printf("%s: pmessage: %s\n", v.Channel, v.Data)
		case error:
			panic(v)
		default:
			log.Println(reflect.TypeOf(reply))
		}
	}
}
