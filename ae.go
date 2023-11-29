package main

import (
	"log"
	"time"

	"golang.org/x/sys/unix"
)

type FeType int

// 两种模式
const (
	AE_READABLE FeType = 1 //读
	AE_WRITABLE FeType = 2 //写
)

type TeType int

const (
	AE_NORMAL TeType = 1
	AE_ONCE   TeType = 2
)

// 文件回调函数  extra一般是client的data
type FileProc func(loop *AeLoop, fd int, extra interface{})

// 时间回调函数
type TimeProc func(loop *AeLoop, id int, extra interface{})

// 文件事件
type AeFileEvent struct {
	fd    int
	mask  FeType   //需要订阅的事件类型  readable 或 writable
	proc  FileProc //回调函数
	extra interface{}
}

// 时间事件
type AeTimeEvent struct {
	id       int
	mask     TeType
	when     int64 //时间点
	interval int64 //事件间隔
	proc     TimeProc
	extra    interface{}
	next     *AeTimeEvent
}

type AeLoop struct {
	/*
		    为什么使用map
			每个client连接到server后都需要注册相应的fileEvent,client结束后会销毁
			新建和销毁非常频繁,如果使用List,每次都需要o(n)来去找到对应的fileEvent,代价比较高
	*/
	FileEvents      map[int]*AeFileEvent
	TimeEvents      *AeTimeEvent
	fileEventFd     int
	timeEventNextId int
	stop            bool
}

// AeEvent 和 epoll常量 之间的映射关系
// AE_READABLE 到 EPOLLIN
// AE_WRITABLE 到 EPOLLOUT
var fe2ep [3]uint32 = [3]uint32{0, unix.EPOLLIN, unix.EPOLLOUT}

// 将fd和事件mask压缩成一个int来做 FileEvents的map的key
// fd和mask可以指定唯一的fileEvent
func getFeKey(fd int, mask FeType) int {
	if mask == AE_READABLE {
		return fd // AE_READABLE就是fd
	} else {
		return fd * -1 // AE_WRITABLE就是fd * -1
	}
}

func (loop *AeLoop) getEpollMask(fd int) uint32 {
	var ev uint32
	if loop.FileEvents[getFeKey(fd, AE_READABLE)] != nil {
		ev |= fe2ep[AE_READABLE]
	}
	if loop.FileEvents[getFeKey(fd, AE_WRITABLE)] != nil {
		ev |= fe2ep[AE_WRITABLE]
	}
	return ev
}

// 添加FileEvent
func (loop *AeLoop) AddFileEvent(fd int, mask FeType, proc FileProc, extra interface{}) {

	/*事件*/
	// epoll ctl
	ev := loop.getEpollMask(fd) //首先确定epoll目前订阅了哪些事件
	if ev&fe2ep[mask] != 0 {
		// event is already registered
		return
	}

	// 设置epoll的op
	op := unix.EPOLL_CTL_ADD //未订阅事件用 add

	if ev != 0 { //已经订阅过事件用 modify
		op = unix.EPOLL_CTL_MOD
	}

	ev |= fe2ep[mask] //增加事件
	// 通过epollCtl将事件订阅进去,epoll是针对fd进行监听的
	err := unix.EpollCtl(loop.fileEventFd, op, fd,
		&unix.EpollEvent{Fd: int32(fd), Events: ev})
	if err != nil {
		log.Printf("epoll ctr err: %v\n", err)
		return
	}

	/*回调*/
	// ae ctl
	// 然后生成AeFileEvent实体
	var fe AeFileEvent
	fe.fd = fd
	fe.mask = mask
	fe.proc = proc
	fe.extra = extra

	// 通过map以fd+mask作为key来将这个fileEvent添加进去
	// 之后,此事件就同时添加到了epoll和ae中
	loop.FileEvents[getFeKey(fd, mask)] = &fe
	log.Printf("ae add file event fd: %v\n", fd, mask)
}

// 删除fileEvent
func (loop *AeLoop) RemoveFileEvent(fd int, mask FeType) {
	// epoll ctl
	op := unix.EPOLL_CTL_DEL    //删除
	ev := loop.getEpollMask(fd) //获取现有event的内容
	//将要删除的事件从ev中拿出来
	ev &= ^fe2ep[mask] //去除要删除的事件后,还剩没剩下东西

	if ev != 0 { //如果还有东西,修改
		op = unix.EPOLL_CTL_MOD
	}

	// 在epoll中处理掉
	err := unix.EpollCtl(loop.fileEventFd, op, fd, &unix.EpollEvent{Fd: int32(fd), Events: ev})
	if err != nil {
		log.Printf("epoll del err: %v\n", err)
	}

	// ae ctl
	// 然后在ae中添加
	loop.FileEvents[getFeKey(fd, mask)] = nil
	log.Printf("ae remove file event fd: %v, mask: %v\n", fd, mask)
}

func GetMsTime() int64 {
	return time.Now().UnixNano() / 1e6
}

// 新增时间事件
func (loop *AeLoop) addTimeEvent(mask TeType, interval int64, proc TimeProc, extra interface{}) int {
	id := loop.timeEventNextId //确认添加的timeEvent的id
	loop.timeEventNextId++

	// init一个AeTimeEvent实体
	var te AeTimeEvent
	te.id = id
	te.mask = mask
	te.interval = interval
	te.when = GetMsTime() + interval
	te.proc = proc
	te.extra = extra
	te.next = loop.TimeEvents //通过头插法放到aeLoop里面
	loop.TimeEvents = &te

	return id //返回id
}

// 删除时间事件
func (loop *AeLoop) RemoveTimeEvent(id int) {
	p := loop.TimeEvents
	var pre *AeTimeEvent

	for p != nil {
		if p.id == id {
			if pre == nil {
				loop.TimeEvents = p.next
			} else {
				pre.next = p.next //删掉p
			}
			p.next = nil
			break
		}
		pre = p
		p = p.next
	}
}

func AeLoopCreate() (*AeLoop, error) {
	epollFd, err := unix.EpollCreate1(0)

	if err != nil {
		return nil, err
	}

	return &AeLoop{
		FileEvents:      make(map[int]*AeFileEvent),
		fileEventFd:     epollFd,
		timeEventNextId: 1,
		stop:            false,
	}, nil
}

func (loop *AeLoop) nearestTime() int64 {
	var nearest int64 = GetMsTime() + 1000

	p := loop.TimeEvents

	for p != nil {
		if p.when < nearest {
			nearest = p.when
		}
		p = p.next
	}
	return nearest
}

/*
timeEvent 需要在最近的time到来的时候,触发timeEvent
fileEvent 需要依赖epoll去唤醒,这会造成阻塞,影响timeEvent的触发,因此需要指定epoll的最长等待时间
*/
func (loop *AeLoop) AeWait() (tes []*AeTimeEvent, fes []*AeFileEvent) {
	// 最近一个timeEvent到来的时间 - 现在的时间
	timeout := loop.nearestTime() - GetMsTime()
	// timeEvent已经到了
	if timeout <= 0 {
		timeout = 10 // 至少等待10ms  (让他多等10ms)
	}

	var events [128]unix.EpollEvent
	// 两轮timeEvent内有n个fileEvent被触发
	n, err := unix.EpollWait(loop.fileEventFd, events[:], int(timeout))
	if err != nil {
		log.Printf("epoll wait warnning: %v\n", err)
	}

	if n > 0 {
		log.Printf("ae get %v epoll events\n", n)
	}

	// collect file events
	for i := 0; i < n; i++ {
		// 返回的event本身也是一个mask,因此需要位运算去确认是哪一种事件
		if events[i].Events&unix.EPOLLIN != 0 {
			fe := loop.FileEvents[getFeKey(int(events[i].Fd), AE_READABLE)]
			if fe != nil {
				fes = append(fes, fe)
			}
		}
		if events[i].Events&unix.EPOLLOUT != 0 {
			fe := loop.FileEvents[getFeKey(int(events[i].Fd), AE_WRITABLE)]
			if fe != nil {
				fes = append(fes, fe)
			}
		}
	}

	// collect time events
	now := GetMsTime()
	p := loop.TimeEvents
	if p != nil {
		if p.when <= now {
			tes = append(tes, p)
		}
		p = p.next
	}
	return

}

func (loop *AeLoop) AeProcess(tes []*AeTimeEvent, fes []*AeFileEvent) {
	// 遍历事件
	for _, te := range tes {
		// 每个事件都调用他们的回调函数
		te.proc(loop, te.id, te.extra)
		if te.mask == AE_ONCE { // 执行一次的,直接将事件remove掉
			loop.RemoveTimeEvent(te.id)
		} else { //循环执行的,需要更新when(下一次执行时间)
			te.when = GetMsTime() + te.interval
		}
	}

	if len(fes) > 0 {
		log.Printf("ae is processing file events")
		for _, fe := range fes {
			fe.proc(loop, fe.fd, fe.extra) //fileEvent直接调他们的回调函数即可
		}
	}
}

func (loop *AeLoop) AeMain() {
	for loop.stop != true { // 当有不可恢复的错误时才去终止
		// wait出ready的timeEvent和fileEvent事件
		tes, fes := loop.AeWait()
		// 处理这两种事件
		loop.AeProcess(tes, fes)
	}
}
