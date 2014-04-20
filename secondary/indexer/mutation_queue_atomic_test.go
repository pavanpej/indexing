package indexer

import (
	"reflect"
	"testing"
	"time"

	"github.com/couchbase/indexing/secondary/common"
)

func TestBasicsA(t *testing.T) {

	q := NewAtomicMutationQueue(1)

	if q == nil {
		t.Errorf("expected new queue allocation to work")
	}

	m := &common.Mutation{Vbucket: 0,
		Seqno: 1}

	q.Enqueue(m, 0)
	checkSizeA(t, q, 0, 1)

	m1 := q.DequeueSingleElement(0)
	checkItemA(t, m, m1)
	checkSizeA(t, q, 0, 0)

	m2 := &common.Mutation{Vbucket: 0,
		Seqno: 2}

	q.Enqueue(m, 0)
	q.Enqueue(m2, 0)
	checkSizeA(t, q, 0, 2)

	m1 = q.DequeueSingleElement(0)
	checkSizeA(t, q, 0, 1)
	checkItemA(t, m, m1)

	m1 = q.DequeueSingleElement(0)
	checkSizeA(t, q, 0, 0)
	checkItemA(t, m2, m1)

}

func checkSizeA(t *testing.T, q MutationQueue, v uint16, s int64) {

	r := q.GetSize(v)
	if r != s {
		t.Errorf("expected queue size %v doesn't match returned size %v", s, r)
	}
}

func checkItemA(t *testing.T, m1 *common.Mutation, m2 *common.Mutation) {
	if !reflect.DeepEqual(m1, m2) {
		t.Errorf("Item returned after dequeue doesn't match enqueued item")
	}
}

func TestSizeA(t *testing.T) {

	q := NewAtomicMutationQueue(1)

	m := make([]*common.Mutation, 10000)
	for i := 0; i < 10000; i++ {
		m[i] = &common.Mutation{Vbucket: 0,
			Seqno: uint64(i)}
		q.Enqueue(m[i], 0)
	}
	checkSizeA(t, q, 0, 10000)

	for i := 0; i < 10000; i++ {
		p := q.DequeueSingleElement(0)
		checkItemA(t, p, m[i])
	}
	checkSizeA(t, q, 0, 0)

}

func TestSizeWithFreelistA(t *testing.T) {

	q := NewAtomicMutationQueue(1)

	m := make([]*common.Mutation, 10000)
	for i := 0; i < 10000; i++ {
		m[i] = &common.Mutation{Vbucket: 0,
			Seqno: uint64(i)}
		q.Enqueue(m[i], 0)
		if (i+1)%100 == 0 {
			checkSizeA(t, q, 0, 100)
			for j := 0; j < 100; j++ {
				p := q.DequeueSingleElement(0)
				checkItemA(t, p, m[(i-99)+j])
			}
			checkSizeA(t, q, 0, 0)
		}
	}
}

func TestDequeueUptoSeqnoA(t *testing.T) {

	q := NewAtomicMutationQueue(1)

	m := make([]*common.Mutation, 10)
	//multiple items with dup seqno
	m[0] = &common.Mutation{Vbucket: 0,
		Seqno: 1}
	m[1] = &common.Mutation{Vbucket: 0,
		Seqno: 1}
	m[2] = &common.Mutation{Vbucket: 0,
		Seqno: 2}

	q.Enqueue(m[0], 0)
	q.Enqueue(m[1], 0)
	q.Enqueue(m[2], 0)
	checkSizeA(t, q, 0, 3)

	ch, err := q.DequeueUptoSeqno(0, 1)

	if err != nil {
		t.Errorf("DequeueUptoSeqno returned error")
	}

	i := 0
	for p := range ch {
		checkItemA(t, m[i], p)
		i++
	}
	checkSizeA(t, q, 0, 1)

	//no-op
	ch, err = q.DequeueUptoSeqno(0, 1)
	for p := range ch {
		checkItemA(t, m[2], p)
	}
	checkSizeA(t, q, 0, 1)

	//one more
	m[3] = &common.Mutation{Vbucket: 0,
		Seqno: 3}
	q.Enqueue(m[3], 0)
	ch, err = q.DequeueUptoSeqno(0, 2)
	for p := range ch {
		checkItemA(t, m[2], p)
	}
	checkSizeA(t, q, 0, 1)

	//check if blocking is working
	ch, err = q.DequeueUptoSeqno(0, 3)

	go func() {
		time.Sleep(100 * time.Millisecond)
		m[4] = &common.Mutation{Vbucket: 0,
			Seqno: 3}
		q.Enqueue(m[4], 0)
		m[5] = &common.Mutation{Vbucket: 0,
			Seqno: 3}
		q.Enqueue(m[5], 0)
		m[6] = &common.Mutation{Vbucket: 0,
			Seqno: 4}
		q.Enqueue(m[6], 0)
	}()

	i = 3
	for p := range ch {
		checkItemA(t, m[i], p)
		i++
	}

	checkSizeA(t, q, 0, 1)
}

func TestDequeueA(t *testing.T) {

	q := NewAtomicMutationQueue(1)

	mut := make([]*common.Mutation, 10)
	for i := 0; i < 10; i++ {
		mut[i] = &common.Mutation{Vbucket: 0,
			Seqno: uint64(i / 2)}
	}
	checkSizeA(t, q, 0, 0)

	//start blocking dequeue call
	ch, stop, _ := q.Dequeue(0)
	go func() {
		for _, m := range mut {
			q.Enqueue(m, 0)
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		stop <- true
	}()

	i := 0
	for p := range ch {
		checkItemA(t, mut[i], p)
		i++
	}
	checkSizeA(t, q, 0, 0)

}

func TestMultipleVbucketsA(t *testing.T) {

	q := NewAtomicMutationQueue(3)

	mut := make([]*common.Mutation, 15)
	for i := 0; i < 15; i++ {
		mut[i] = &common.Mutation{Vbucket: 0,
			Seqno: uint64(i)}
	}
	checkSizeA(t, q, 0, 0)
	checkSizeA(t, q, 1, 0)
	checkSizeA(t, q, 2, 0)

	for i := 0; i < 5; i++ {
		q.Enqueue(mut[i], 0)
		q.Enqueue(mut[i+5], 1)
		q.Enqueue(mut[i+10], 2)
	}
	checkSizeA(t, q, 0, 5)
	checkSizeA(t, q, 1, 5)
	checkSizeA(t, q, 2, 5)

	var p *common.Mutation
	for i := 0; i < 5; i++ {
		p = q.DequeueSingleElement(0)
		checkItemA(t, p, mut[i])
		p = q.DequeueSingleElement(1)
		checkItemA(t, p, mut[i+5])
		p = q.DequeueSingleElement(2)
		checkItemA(t, p, mut[i+10])
	}

}

func BenchmarkEnqueueA(b *testing.B) {

	q := NewAtomicMutationQueue(1)

	mut := make([]*common.Mutation, b.N)
	for i := 0; i < b.N; i++ {
		mut[i] = &common.Mutation{Vbucket: 0,
			Seqno: uint64(i)}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Enqueue(mut[i], 0)
	}

}
func BenchmarkDequeueA(b *testing.B) {

	q := NewAtomicMutationQueue(1)

	mut := make([]*common.Mutation, b.N)
	for i := 0; i < b.N; i++ {
		mut[i] = &common.Mutation{Vbucket: 0,
			Seqno: uint64(i)}
	}
	for _, m := range mut {
		q.Enqueue(m, 0)
	}

	//start blocking dequeue call
	ch, stop, _ := q.Dequeue(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		<-ch
	}
	stop <- true
}

func BenchmarkSingleVbucketA(b *testing.B) {

	q := NewAtomicMutationQueue(1)

	mut := make([]*common.Mutation, b.N)
	for i := 0; i < b.N; i++ {
		mut[i] = &common.Mutation{Vbucket: 0,
			Seqno: uint64(i)}
	}

	ch, stop, _ := q.Dequeue(0)

	b.ResetTimer()
	//start blocking dequeue call
	go func() {
		for i := 0; i < b.N; i++ {
			q.Enqueue(mut[i], 0)
		}
	}()

	for i := 0; i < b.N; i++ {
		<-ch
	}
	stop <- true
}
