
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

#line 3 "/sessions/great-funny-faraday/go/pkg/mod/github.com/marcboeker/go-duckdb@v1.8.3/scalarUDF.go"

#include <duckdb.h>

void scalar_udf_callback(duckdb_function_info, duckdb_data_chunk, duckdb_vector);
void udf_delete_callback(void *);

typedef void (*scalar_udf_callback_t)(duckdb_function_info, duckdb_data_chunk, duckdb_vector);

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
_cgo_b59e10b900ff_Cfunc_duckdb_add_scalar_function_to_set(void *v)
{
	struct {
		struct _duckdb_scalar_function_set* p0;
		struct _duckdb_scalar_function* p1;
		duckdb_state r;
		char __pad20[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_add_scalar_function_to_set(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_create_scalar_function(void *v)
{
	struct {
		duckdb_scalar_function r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_create_scalar_function();
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_create_scalar_function_set(void *v)
{
	struct {
		char const* p0;
		duckdb_scalar_function_set r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_create_scalar_function_set(_cgo_a->p0);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_destroy_scalar_function(void *v)
{
	struct {
		duckdb_scalar_function* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_destroy_scalar_function(_cgo_a->p0);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_destroy_scalar_function_set(void *v)
{
	struct {
		duckdb_scalar_function_set* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_destroy_scalar_function_set(_cgo_a->p0);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_register_scalar_function(void *v)
{
	struct {
		struct _duckdb_connection* p0;
		struct _duckdb_scalar_function* p1;
		duckdb_state r;
		char __pad20[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_register_scalar_function(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_register_scalar_function_set(void *v)
{
	struct {
		struct _duckdb_connection* p0;
		struct _duckdb_scalar_function_set* p1;
		duckdb_state r;
		char __pad20[4];
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = duckdb_register_scalar_function_set(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_scalar_function_add_parameter(void *v)
{
	struct {
		struct _duckdb_scalar_function* p0;
		struct _duckdb_logical_type* p1;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_scalar_function_add_parameter(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_scalar_function_get_extra_info(void *v)
{
	struct {
		struct _duckdb_function_info* p0;
		void* r;
	} __attribute__((__packed__)) *_cgo_a = v;
	char *_cgo_stktop = _cgo_topofstack();
	__typeof__(_cgo_a->r) _cgo_r;
	_cgo_tsan_acquire();
	_cgo_r = (__typeof__(_cgo_a->r)) duckdb_scalar_function_get_extra_info(_cgo_a->p0);
	_cgo_tsan_release();
	_cgo_a = (void*)((char*)_cgo_a + (_cgo_topofstack() - _cgo_stktop));
	_cgo_a->r = _cgo_r;
	_cgo_msan_write(&_cgo_a->r, sizeof(_cgo_a->r));
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_scalar_function_set_extra_info(void *v)
{
	struct {
		struct _duckdb_scalar_function* p0;
		void* p1;
		void* p2;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_scalar_function_set_extra_info(_cgo_a->p0, _cgo_a->p1, _cgo_a->p2);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_scalar_function_set_function(void *v)
{
	struct {
		struct _duckdb_scalar_function* p0;
		void* p1;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_scalar_function_set_function(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_scalar_function_set_name(void *v)
{
	struct {
		struct _duckdb_scalar_function* p0;
		char const* p1;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_scalar_function_set_name(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_scalar_function_set_return_type(void *v)
{
	struct {
		struct _duckdb_scalar_function* p0;
		struct _duckdb_logical_type* p1;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_scalar_function_set_return_type(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_scalar_function_set_special_handling(void *v)
{
	struct {
		struct _duckdb_scalar_function* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_scalar_function_set_special_handling(_cgo_a->p0);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_scalar_function_set_varargs(void *v)
{
	struct {
		struct _duckdb_scalar_function* p0;
		struct _duckdb_logical_type* p1;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_scalar_function_set_varargs(_cgo_a->p0, _cgo_a->p1);
	_cgo_tsan_release();
}

CGO_NO_SANITIZE_THREAD
void
_cgo_b59e10b900ff_Cfunc_duckdb_scalar_function_set_volatile(void *v)
{
	struct {
		struct _duckdb_scalar_function* p0;
	} __attribute__((__packed__)) *_cgo_a = v;
	_cgo_tsan_acquire();
	duckdb_scalar_function_set_volatile(_cgo_a->p0);
	_cgo_tsan_release();
}

