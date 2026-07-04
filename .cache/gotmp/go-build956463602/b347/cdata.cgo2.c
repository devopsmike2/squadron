
#line 1 "cgo-builtin-prolog"
#include <stddef.h>

/* Define intgo when compiling with GCC.  */
typedef ptrdiff_t intgo;

#define GO_CGO_GOSTRING_TYPEDEF
typedef struct { const char *p; intgo n; } _GoString_;
typedef struct { char *p; intgo n; intgo c; } _GoBytes_;
_GoString_ GoString(char *p);
_GoString_ GoStringN(char *p, int l);
_GoBytes_ GoBytes(void *p, int n);
char *CString(_GoString_);
void *CBytes(_GoBytes_);
void *_CMalloc(size_t);

__attribute__ ((unused))
static size_t _GoStringLen(_GoString_ s) { return (size_t)s.n; }

__attribute__ ((unused))
static const char *_GoStringPtr(_GoString_ s) { return s.p; }

#line 24 "/sessions/great-funny-faraday/go/pkg/mod/github.com/apache/arrow-go/v18@v18.0.0/arrow/cdata/cdata.go"
 #include "arrow/c/abi.h"
 #include "arrow/c/helpers.h"
 #include <stdlib.h>
 int stream_get_schema(struct ArrowArrayStream* st, struct ArrowSchema* out) { return st->get_schema(st, out); }
 int stream_get_next(struct ArrowArrayStream* st, struct ArrowArray* out) { return st->get_next(st, out); }
 const char* stream_get_last_error(struct ArrowArrayStream* st) { return st->get_last_error(st); }
 struct ArrowArray* get_arr() {
	struct ArrowArray* out = (struct ArrowArray*)(malloc(sizeof(struct ArrowArray)));
	memset(out, 0, sizeof(struct ArrowArray));
	return out;
 }
 struct ArrowArrayStream* get_stream() {
	struct ArrowArrayStream* out = (struct ArrowArrayStream*)malloc(sizeof(struct ArrowArrayStream));
	memset(out, 0, sizeof(struct ArrowArrayStream));
	return out;
 }


#line 1 "cgo-generated-wrapper"


#line 1 "cgo-gcc-prolog"
/*
  If x and y are not equal, the type will be invalid
  (have a negative array count) and an inscrutable error will come
  out of the compiler and hopefully mention "name".
*/
#define __cgo_compile_assert_eq(x, y, name) typedef char name[(x-y)*(x-y)*-2UL+1UL];

/* Check at compile time that the sizes we use match our expectations. */
#define __cgo_size_assert(t, n) __cgo_compile_assert_eq(sizeof(t), (size_t)n, _cgo_sizeof_##t##_is_not_##n)

__cgo_size_assert(char, 1)
__cgo_size_assert(short, 2)
__cgo_size_assert(int, 4)
typedef long long __cgo_long_long;
__cgo_size_assert(__cgo_long_long, 8)
__cgo_size_assert(float, 4)
__cgo_size_assert(double, 8)

extern char* _cgo_topofstack(void);

/*
  We use packed structs, but they are always aligned.
  The pragmas and address-of-packed-member are only recognized as warning
  groups in clang 4.0+, so ignore unknown pragmas first.
*/
#pragma GCC diagnostic ignored "-Wunknown-pragmas"
#pragma GCC diagnostic ignored "-Wpragmas"
#pragma GCC diagnostic ignored "-Waddress-of-packed-member"
#pragma GCC diagnostic ignored "-Wunknown-warning-option"
#pragma GCC diagnostic ignored "-Wunaligned-access"

#include <errno.h>
#include <string.h>


#define CGO_NO_SANITIZE_THREAD
#define _cgo_tsan_acquire()
#define _cgo_tsan_release()


#define _cgo_msan_write(addr, sz)

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_ArrowArrayIsReleased(void *v)
{
	struct {
		struct ArrowArray const* p0;
		int r;
		char __pad12[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = ArrowArrayIsReleased(_cgo_a->p0);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_ArrowArrayMove(void *v)
{
	struct {
		struct ArrowArray* p0;
		struct ArrowArray* p1;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	ArrowArrayMove(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_ArrowArrayRelease(void *v)
{
	struct {
		struct ArrowArray* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	ArrowArrayRelease(_cgo_a->p0);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_ArrowArrayStreamMove(void *v)
{
	struct {
		struct ArrowArrayStream* p0;
		struct ArrowArrayStream* p1;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	ArrowArrayStreamMove(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_ArrowArrayStreamRelease(void *v)
{
	struct {
		struct ArrowArrayStream* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	ArrowArrayStreamRelease(_cgo_a->p0);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_ArrowSchemaRelease(void *v)
{
	struct {
		struct ArrowSchema* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	ArrowSchemaRelease(_cgo_a->p0);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_free(void *v)
{
	struct {
		void* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	free(_cgo_a->p0);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_get_arr(void *v)
{
	struct {
		struct ArrowArray* r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = (__typeof__(_cgo_a->r)) get_arr();
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_get_stream(void *v)
{
	struct {
		struct ArrowArrayStream* r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = (__typeof__(_cgo_a->r)) get_stream();
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_stream_get_last_error(void *v)
{
	struct {
		struct ArrowArrayStream* p0;
		char const* r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = (__typeof__(_cgo_a->r)) stream_get_last_error(_cgo_a->p0);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_stream_get_next(void *v)
{
	struct {
		struct ArrowArrayStream* p0;
		struct ArrowArray* p1;
		int r;
		char __pad20[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = stream_get_next(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_95ddaae2a49e_Cfunc_stream_get_schema(void *v)
{
	struct {
		struct ArrowArrayStream* p0;
		struct ArrowSchema* p1;
		int r;
		char __pad20[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = stream_get_schema(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

