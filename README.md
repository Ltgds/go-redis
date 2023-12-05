# go-redis
基于go实现redis的核心流程和数据结构


`ae.go` 实现aeEventLoop、epoll等代码 

`conf.go` 实现redis-server一些配置，比如端口、最大连接数等

`dict.go`，`list.go`，`zset.go` 实现redis数据结构

`obj.go` 实现redisObject

`net.go` 基于socket accept等系统调用

`godis.go` 主代码
