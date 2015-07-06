package raftis

import (
	"fmt"
	"github.com/jbooth/raftis/config"
	log "github.com/jbooth/raftis/rlog"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

func NewClusterMember(c *config.ClusterConfig, lg *log.Logger) (*ClusterMember, error) {
	// just set up hostConns all at once for now
	slotHosts := make(map[int32][]config.Host)
	for _, shard := range c.Shards {
		for _, slot := range shard.Slots {
			slotHosts[int32(slot)] = shard.Hosts
		}
	}
	hostConns := make(map[string]hostConn)
	for _, shard := range c.Shards {
		for _, host := range shard.Hosts {
			var err error = nil
			hostConns[host], err = newHostConn(host)
			if err != nil {
				hostConns[host].markErr()
				lg.Printf("Error initially connecting to %s : %s", host, err)
			}
		}
	}
	conn, err := NewPassThru(host)
	return &ClusterMember{
		lg,
		&sync.RWMutex{},
		c,
		slotHosts,
		hostConns,
	}, nil

}

type ClusterMember struct {
	lg        *log.Logger
	l         *sync.RWMutex
	c         *config.ClusterConfig
	slotHosts map[int32][]config.Host
	hostConns map[string]*hostConn
}

type hostConn struct {
	p           *PassthruConn
	host        string
	lastErrTime *int64
}

func (h *hostConn) lastErrTime() int64 {
	return atomic.LoadInt64(h.lastErrTime)
}

func (h *hostConn) markErr() {
	atomic.StoreInt64(h.lastErrTime, time.Now().Unix())
	h.p.Close()
}

// should hold cluster writelock while calling this to insure visibility
func (h *hostConn) renew() (err error) {
	h.p, err = NewPassThru(h.host)
	if err != nil {
		h.lastErrTime = 0
	} else {
		h.lastErrTime = time.Unix().Now()
	}
}

func newHostConn(host string) (*hostConn, err) {
	p, err := NewPassThru(host)
	if err != nil {
		return nil, err
	}
	return &hostConn{p, host, -1}
}

func (c *ClusterMember) HasKey(cmdName string, args [][]byte) (bool, error) {
	if cmdName == "PING" {
		// pings always evaluated locally
		return true, nil
	}
	if len(args) == 0 {
		return false, fmt.Errorf("HasKey Can't handle 0-arg commands other than PING.  Cmd: %s", cmdName)
	}
	var key []byte
	if cmdName == "EVAL" {
		// first arg is command name, 2nd is key
		key = args[1]
	} else {
		key = args[0]
	}
	s := c.slotForKey(key)
	hosts, ok := c.slotHosts[s]
	if !ok {
		return false, fmt.Errorf("No hosts for slot %d", s)
	}
	for _, h := range hosts {
		if h.RedisAddr == c.c.Me.RedisAddr {
			return true, nil
		}
	}
	return false, nil
}

func (c *ClusterMember) ForwardCommand(cmdName string, args [][]byte) (io.WriterTo, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("Can't forward command %s, need at least 1 arg for key!", cmdName)
	}
	conn, err := c.getConnForKey(args[0])
	if err != nil {
		return nil, err
	}
	return conn.Command(cmdName, args)
}

func (c *ClusterMember) getConnForKey(key []byte) (*PassthruConn, error) {
	c.l.RLock()
	defer c.l.RUnlock()
	slot := c.slotForKey(key)
	hosts, ok := c.slotHosts[slot]
	//c.lg.Printf("Choosing from hosts %+v for key %s slot %d", hosts, key, slot)
	if !ok {
		return nil, fmt.Errorf("No hosts configured for slot %d from key %s", slot, key)
	}
	hostsByGroup := make(map[string]config.Host)
	for _, host := range hosts {
		if host.RedisAddr == c.c.Me.RedisAddr {
			return nil, fmt.Errorf("Can't passthru to localhost!  Use the right interface.")
		}
		hostsByGroup[host.Group] = host
	}
	// try to favor same group
	sameGroup, hasSameGroup := hostsByGroup[c.c.Me.Group]
	var err error
	if hasSameGroup {
		hostConn := c.hostConns[host]
		if hostConn != nil {

		}
		//c.lg.Printf("Connecting to host from same group %+v", sameGroup)
		sameGroupConn, err := c.getConnForHost(sameGroup.RedisAddr)
		if err == nil {
			//c.lg.Printf("returning from same group")
			return sameGroupConn, nil
		} else {
			if err != hostMarkedDown {
				c.lg.Errorf("Error connecting to host %s for slot %d : %s", sameGroup.RedisAddr, slot, err.Error())
			}
		}
	}
	// otherwise, randomly iterate till we find one that works
	for _, h := range hostsByGroup {
		conn, err := c.getConnForHost(h.RedisAddr)
		if err == nil {
			return conn, nil
		} else {
			if err != hostMarkedDown {
				c.lg.Errorf("Error connecting to host %+v for slot %d : %s", h, slot, err)
			}
		}
	}
	return nil, fmt.Errorf("Couldn't find any hosts up for slot %d, key %s, hosts are %+v, error from last connect attempt: %s", slot, key, hosts, err)

}

var hostMarkedDown error = fmt.Errorf("Host down")

// assumes Rlock is held, returns error if this host is marked down
func (c *ClusterMember) getConnForHost(host string) (*PassthruConn, error) {
	conn, ok := c.hostConns[host]
	if ok {
		lastErrTime := conn.lastErrTime()
		if lastErrTime > 0 {
			if time.Now().Unix()-lastErrTime > 360 {
				// if older than 5 minutes renew
				// switch to writelock
				c.l.RUnlock()
				c.l.Lock()
				defer func() {
					c.l.Unlock()
					c.l.RLock()
				}()
				// double check after writelock
				if time.Now().Unix()-conn.lastErrTime() > 0 {
					conn.renew()
				}
				return conn, nil
			} else {
				// if not, return error
				return nil, hostMarkedDown
			}

		}
		return conn, nil
	} else {
		return nil, fmt.Errorf("No conn identified for host %s")
	}
}

func (c *ClusterMember) slotForKey(key []byte) int32 {
	h := hash(key)
	if h < 0 {
		h = -h
	}
	ret := h % int32(c.c.NumSlots)
	return ret
}

func hash(key []byte) int32 {
	if key == nil {
		return 0
	}
	sum := int32(0)
	for i := 0; i < len(key); i++ {
		sum = (sum * 17) + int32(key[i])
	}
	return sum
}
