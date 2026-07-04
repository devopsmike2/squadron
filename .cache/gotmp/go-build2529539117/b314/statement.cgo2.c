
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

#line 3 "/sessions/great-funny-faraday/go/pkg/mod/github.com/marcboeker/go-duckdb@v1.8.3/statement.go"

#include <duckdb.h>

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
_cgo_b59e10b900ff_Cfunc_duckdb_bind_blob(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		const void* p2;
		idx_t p3;
		duckdb_state r;
		char __pad36[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_blob(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2, _cgo_a->p3);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_boolean(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		_Bool p2;
		char __pad17[7];
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_boolean(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_double(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		double p2;
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_double(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_float(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		float p2;
		char __pad20[4];
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_float(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_hugeint(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		duckdb_hugeint p2;
		duckdb_state r;
		char __pad36[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_hugeint(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_int16(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		int16_t p2;
		char __pad18[6];
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_int16(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_int32(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		int32_t p2;
		char __pad20[4];
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_int32(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_int64(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		int64_t p2;
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_int64(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_int8(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		int8_t p2;
		char __pad17[7];
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_int8(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_interval(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		duckdb_interval p2;
		duckdb_state r;
		char __pad36[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_interval(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_null(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		duckdb_state r;
		char __pad20[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_null(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_timestamp(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		duckdb_timestamp p2;
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_timestamp(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_uint16(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		uint16_t p2;
		char __pad18[6];
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_uint16(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_uint32(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		uint32_t p2;
		char __pad20[4];
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_uint32(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_uint64(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		uint64_t p2;
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_uint64(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_uint8(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		uint8_t p2;
		char __pad17[7];
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_uint8(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_bind_varchar(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		char const* p2;
		duckdb_state r;
		char __pad28[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_bind_varchar(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_destroy_pending(void *v)
{
	struct {
		duckdb_pending_result* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_destroy_pending(_cgo_a->p0);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_execute_pending(void *v)
{
	struct {
		struct _duckdb_pending_result* p0;
		duckdb_result* p1;
		duckdb_state r;
		char __pad20[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_execute_pending(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_interrupt(void *v)
{
	struct {
		struct _duckdb_connection* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_interrupt(_cgo_a->p0);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_nparams(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_nparams(_cgo_a->p0);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_parameter_name(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		idx_t p1;
		char const* r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = (__typeof__(_cgo_a->r)) duckdb_parameter_name(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_pending_error(void *v)
{
	struct {
		struct _duckdb_pending_result* p0;
		char const* r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = (__typeof__(_cgo_a->r)) duckdb_pending_error(_cgo_a->p0);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_pending_prepared(void *v)
{
	struct {
		struct _duckdb_prepared_statement* p0;
		duckdb_pending_result* p1;
		duckdb_state r;
		char __pad20[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_pending_prepared(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_result_error(void *v)
{
	struct {
		duckdb_result* p0;
		char const* r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = (__typeof__(_cgo_a->r)) duckdb_result_error(_cgo_a->p0);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_value_int64(void *v)
{
	struct {
		duckdb_result* p0;
		idx_t p1;
		idx_t p2;
		int64_t r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_value_int64(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

