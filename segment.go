package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	lru "github.com/hashicorp/golang-lru/v2"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

type ChunkType = byte
type SegmentID = uint32

// 定义一个枚举值
const (
	ChunkTypeFull ChunkType = iota
	ChunkTypeFirst
	ChunkTypeMiddle
	ChunkTypeLast
)

var (
	ErrClosed     = errors.New("the segment file is closed")
	ErrInvalidCRC = errors.New("invalid crc, the data may be corrupted")
)

const (
	// 7 Bytes
	// Checksum Length Type
	//    4      2     1
	chunkHeaderSize = 7

	// 32 KB
	blockSize = 32 * KB

	fileModePerm = 0644
)

type segment struct {
	id                 SegmentID
	fd                 *os.File
	currentBlockNumber uint32
	currentBlockSize   uint32
	closed             bool
	cache              *lru.Cache[uint64, []byte]
	header             []byte
	blockPool          sync.Pool
}

func (seg *segment) NewReader() *segmentReader {
	return &segmentReader{
		segment:     seg,
		blockNumber: 0,
		chunkOffset: 0,
	}
}

func (seg *segment) Sync() error {
	if seg.closed {
		return nil
	}
	return seg.fd.Sync()
}

func (seg *segment) Remove() error {
	if !seg.closed {
		seg.closed = true
		_ = seg.fd.Close()
	}

	return os.Remove(seg.fd.Name())
}

func (seg *segment) Close() error {
	if seg.closed {
		return nil
	}

	seg.closed = true
	return seg.fd.Close()
}

func (seg *segment) Size() int64 {
	return int64(seg.currentBlockNumber*blockSize + seg.currentBlockSize)
}

func (seg *segment) Write(data []byte) (*ChunkPosition, error) {
	if seg.closed {
		return nil, ErrClosed
	}

	// The left block space is not enough for a chunk header
	if seg.currentBlockSize+chunkHeaderSize >= blockSize {
		// padding if necessary
		if seg.currentBlockSize < blockSize {
			padding := make([]byte, blockSize-seg.currentBlockSize)
			if _, err := seg.fd.Write(padding); err != nil {
				return nil, err
			}
		}

		// A new block, clear the current block size.
		seg.currentBlockNumber += 1
		seg.currentBlockSize = 0
	}

	// the start position(for read operation)
	position := &ChunkPosition{
		SegmentId:   seg.id,
		BlockNumber: seg.currentBlockNumber,
		ChunkOffset: int64(seg.currentBlockSize),
	}
	dataSize := uint32(len(data))
	// The entire chunk can fit into the block.
	if seg.currentBlockSize+dataSize+chunkHeaderSize <= blockSize {
		err := seg.writeInternal(data, ChunkTypeFull)
		if err != nil {
			return nil, err
		}
		position.ChunkSize = dataSize + chunkHeaderSize
		return position, nil
	}

	// If the size of the data exceeds the size of the block,
	// the data should be written to the block in batches.
	var leftSize = dataSize
	var blockCount uint32 = 0
	for leftSize > 0 {
		chunkSize := blockSize - seg.currentBlockSize - chunkHeaderSize
		if chunkSize > leftSize {
			chunkSize = leftSize
		}
		chunk := make([]byte, chunkSize)

		var end = dataSize - leftSize + chunkSize
		if end > dataSize {
			end = dataSize
		}

		copy(chunk[:], data[dataSize-leftSize:end])

		// write the chunks
		var err error
		if leftSize == dataSize {
			// First Chunk
			err = seg.writeInternal(chunk, ChunkTypeFirst)
		} else if leftSize == chunkSize {
			// Last Chunk
			err = seg.writeInternal(chunk, ChunkTypeLast)
		} else {
			// Middle Chunk
			err = seg.writeInternal(chunk, ChunkTypeMiddle)
		}
		if err != nil {
			return nil, err
		}
		leftSize -= chunkSize
		blockCount += 1
	}

	position.ChunkSize = blockCount*chunkHeaderSize + dataSize
	return position, nil
}

func (seg *segment) writeInternal(data []byte, chunkType ChunkType) error {
	dataSize := uint32(len(data))

	// Checksum Length Type
	//    4      2     1
	binary.LittleEndian.PutUint16(seg.header[4:6], uint16(dataSize))
	seg.header[6] = chunkType

	sum := crc32.ChecksumIEEE(seg.header[4:])
	sum = crc32.Update(sum, crc32.IEEETable, data)
	binary.LittleEndian.PutUint32(seg.header[:4], sum)

	// 追加写入数据
	if _, err := seg.fd.Write(seg.header); err != nil {
		return err
	}
	if _, err := seg.fd.Write(data); err != nil {
		return err
	}

	if seg.currentBlockSize > blockSize {
		panic("wrong! can not exceed the block size")
	}

	// 写入溢出的判断，放在调用 writeInternal地方了
	seg.currentBlockSize += dataSize + chunkHeaderSize

	if seg.currentBlockSize == blockSize {
		seg.currentBlockNumber += 1
		seg.currentBlockSize = 0
	}
	return nil

}

type segmentReader struct {
	segment     *segment
	blockNumber uint32
	chunkOffset int64
}

type blockAndHeader struct {
	block  []byte
	header []byte
}

type ChunkPosition struct {
	SegmentId   SegmentID
	BlockNumber uint32
	ChunkOffset int64
	ChunkSize   uint32
}

//func SegmentFileName(dirPath string, extName string, id SegmentID) string {
//	return filepath.Join(dirPath, fmt.Sprintf("%09d"+extName, id))
//}

func newBlockAndHeader() interface{} {
	return &blockAndHeader{
		block:  make([]byte, blockSize),
		header: make([]byte, chunkHeaderSize),
	}
}

func openSegmentFile(dirpath, extName string, id uint32, cache *lru.Cache[uint64, []byte]) (*segment, error) {
	fd, err := os.OpenFile(
		SegmentFileName(dirpath, extName, id),
		os.O_CREATE|os.O_RDWR|os.O_APPEND,
		fileModePerm,
	)
	if err != nil {
		return nil, err
	}

	offset, err := fd.Seek(0, io.SeekEnd)
	if err != nil {
		panic(fmt.Errorf("seek to the end of segment file %d%s failed: %v", id, extName, err))
	}

	return &segment{
		id:                 id,
		fd:                 fd,
		cache:              cache,
		header:             make([]byte, chunkHeaderSize),
		blockPool:          sync.Pool{New: newBlockAndHeader},
		currentBlockNumber: uint32(offset / blockSize),
		currentBlockSize:   uint32(offset % blockSize),
	}, nil

}

func (seg *segment) Read(blockNumber uint32, chunkOffset int64) ([]byte, error) {
	value, _, err := seg.readInternal(blockNumber, chunkOffset)
	return value, err
}

func (seg *segment) readInternal(blockNumber uint32, chunkOffset int64) ([]byte, *ChunkPosition, error) {
	if seg.closed {
		return nil, nil, ErrClosed
	}

	var (
		result    []byte
		bh        = seg.blockPool.Get().(*blockAndHeader)
		segSize   = seg.Size()
		nextChunk = &ChunkPosition{SegmentId: seg.id}
	)

	defer func() {
		seg.blockPool.Put(bh)
	}()

	for {
		size := int64(blockSize)
		offset := int64(blockNumber * blockSize)

		if size+offset > segSize {
			size = segSize - offset
		}

		if chunkOffset >= size {
			return nil, nil, io.EOF
		}

		var ok bool
		var cachedBlock []byte

		if seg.cache != nil {
			cachedBlock, ok = seg.cache.Get(seg.getCacheKey(blockNumber))
		}

		if ok {
			copy(bh.block, cachedBlock)
		} else {
			_, err := seg.fd.ReadAt(bh.block[0:size], offset)
			if err != nil {
				return nil, nil, err
			}

			if seg.cache != nil && size == blockSize && len(cachedBlock) == 0 {
				cacheBlock := make([]byte, blockSize)
				copy(cacheBlock, bh.block)
				seg.cache.Add(seg.getCacheKey(blockNumber), cacheBlock)
			}

		}

		copy(bh.header, bh.block[chunkOffset:chunkOffset+chunkHeaderSize])

		length := binary.LittleEndian.Uint16(bh.header[4:6])

		start := chunkOffset + chunkHeaderSize
		result = append(result, bh.block[start:start+int64(length)]...)

		// check sum
		checksumEnd := chunkOffset + chunkHeaderSize + int64(length)
		checksum := crc32.ChecksumIEEE(bh.block[chunkOffset+4 : checksumEnd])
		savedSum := binary.LittleEndian.Uint32(bh.header[:4])
		if savedSum != checksum {
			return nil, nil, ErrInvalidCRC
		}

		chunkType := bh.header[6]

		if chunkType == ChunkTypeFull || chunkType == ChunkTypeLast {
			nextChunk.BlockNumber = blockNumber
			nextChunk.ChunkOffset = checksumEnd
			// If this is the last chunk in the block, and the left block
			// space are paddings, the next chunk should be in the next block.
			if checksumEnd+chunkHeaderSize >= blockSize {
				nextChunk.BlockNumber += 1
				nextChunk.ChunkOffset = 0
			}
			break
		}

		blockNumber += 1
		chunkOffset = 0
	}
	return result, nextChunk, nil

}

func (seg *segment) getCacheKey(blockNumber uint32) uint64 {
	return uint64(seg.id)<<32 | uint64(blockNumber)
}

func (segReader *segmentReader) Next() ([]byte, *ChunkPosition, error) {
	// The segment file is closed
	if segReader.segment.closed {
		return nil, nil, ErrClosed
	}

	// this position describes the current chunk info
	chunkPosition := &ChunkPosition{
		SegmentId:   segReader.segment.id,
		BlockNumber: segReader.blockNumber,
		ChunkOffset: segReader.chunkOffset,
	}

	value, nextChunk, err := segReader.segment.readInternal(
		segReader.blockNumber,
		segReader.chunkOffset,
	)
	if err != nil {
		return nil, nil, err
	}

	// Calculate the chunk size.
	// Remember that the chunk size is just an estimated value,
	// not accurate, so don't use it for any important logic.
	chunkPosition.ChunkSize =
		nextChunk.BlockNumber*blockSize + uint32(nextChunk.ChunkOffset) -
			(segReader.blockNumber*blockSize + uint32(segReader.chunkOffset))

	// update the position
	segReader.blockNumber = nextChunk.BlockNumber
	segReader.chunkOffset = nextChunk.ChunkOffset

	return value, chunkPosition, nil
}

// Encode encodes the chunk position to a byte slice.
// You can decode it by calling wal.DecodeChunkPosition().
func (cp *ChunkPosition) Encode() []byte {
	maxLen := binary.MaxVarintLen32*3 + binary.MaxVarintLen64
	buf := make([]byte, maxLen)

	var index = 0
	// SegmentId
	index += binary.PutUvarint(buf[index:], uint64(cp.SegmentId))
	// BlockNumber
	index += binary.PutUvarint(buf[index:], uint64(cp.BlockNumber))
	// ChunkOffset
	index += binary.PutUvarint(buf[index:], uint64(cp.ChunkOffset))
	// ChunkSize
	index += binary.PutUvarint(buf[index:], uint64(cp.ChunkSize))

	return buf[:index]
}

// DecodeChunkPosition decodes the chunk position from a byte slice.
// You can encode it by calling wal.ChunkPosition.Encode().
func DecodeChunkPosition(buf []byte) *ChunkPosition {
	if len(buf) == 0 {
		return nil
	}

	var index = 0
	// SegmentId
	segmentId, n := binary.Uvarint(buf[index:])
	index += n
	// BlockNumber
	blockNumber, n := binary.Uvarint(buf[index:])
	index += n
	// ChunkOffset
	chunkOffset, n := binary.Uvarint(buf[index:])
	index += n
	// ChunkSize
	chunkSize, n := binary.Uvarint(buf[index:])
	index += n

	return &ChunkPosition{
		SegmentId:   uint32(segmentId),
		BlockNumber: uint32(blockNumber),
		ChunkOffset: int64(chunkOffset),
		ChunkSize:   uint32(chunkSize),
	}
}
