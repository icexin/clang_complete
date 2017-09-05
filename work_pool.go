package main

import "sync"

type pool struct {
	sem  chan struct{}
	wait sync.WaitGroup
}

func newPool(n int) *pool {
	return &pool{
		sem: make(chan struct{}, n),
	}
}

func (p *pool) Run(f func()) {
	p.sem <- struct{}{}
	p.wait.Add(1)
	go func() {
		f()
		p.wait.Done()
		<-p.sem
	}()
}

func (p *pool) Wait() {
	p.wait.Wait()
}
