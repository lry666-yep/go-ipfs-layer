package simplebloom

import (
	"crypto/sha256"

	"github.com/spaolacci/murmur3"
)

type BloomFilter interface {
	Put([]byte)
	PutString(string)

	Has([]byte) bool
	HasString(string) bool

	Close()
	GetBitSet() []int64
}

type MemoryBloomFilter struct {
	k  uint
	bs BitSets
}

func HashData(data []byte, seed uint) uint {
	sha_data := sha256.Sum256(data)
	data = sha_data[:]
	m := murmur3.New64WithSeed(uint32(seed))
	m.Write(data)
	return uint(m.Sum64())
}

// NewMemoryBloomFilter 创建一个内存的bloom filter
func NewMemoryBloomFilter() *MemoryBloomFilter {
	return &MemoryBloomFilter{
		k:  7,
		bs: NewBitSets(10000),
	}
}

func ConstructMemoryBloomFilter(set []int64) *MemoryBloomFilter {
	return &MemoryBloomFilter{
		k:  7,
		bs: set,
	}
}

func (filter *MemoryBloomFilter) GetBitSet() []int64 {
	return filter.bs
}

// Put 添加一条记录
func (filter *MemoryBloomFilter) Put(data []byte) {
	l := uint(len(filter.bs))
	for i := uint(0); i < filter.k; i++ {
		filter.bs.Set(HashData(data, i) % l)
	}
}

// Put 添加一条string记录
func (filter *MemoryBloomFilter) PutString(data string) {
	filter.Put([]byte(data))
}

// Has 推测记录是否已存在
func (filter *MemoryBloomFilter) Has(data []byte) bool {
	l := uint(len(filter.bs))

	for i := uint(0); i < filter.k; i++ {
		if !filter.bs.IsSet(HashData(data, i) % l) {
			return false
		}
	}

	return true
}

// Has 推测记录是否已存在
func (filter *MemoryBloomFilter) HasString(data string) bool {
	return filter.Has([]byte(data))
}

// Close 关闭bloom filter
func (filter *MemoryBloomFilter) Close() {
	filter.bs = nil
}

