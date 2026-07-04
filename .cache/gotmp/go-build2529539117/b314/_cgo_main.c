#include <stddef.h>
int main(int argc __attribute__((unused)), char **argv __attribute__((unused))) { return 0; }
void crosscall2(void(*fn)(void*) __attribute__((unused)), void *a __attribute__((unused)), int c __attribute__((unused)), size_t ctxt __attribute__((unused))) { }
size_t _cgo_wait_runtime_init_done(void) { return 0; }
void _cgo_release_context(size_t ctxt __attribute__((unused))) { }
char* _cgo_topofstack(void) { return (char*)0; }
void _cgo_allocate(void *a __attribute__((unused)), int c __attribute__((unused))) { }
void _cgo_panic(void *a __attribute__((unused)), int c __attribute__((unused))) { }
void _cgo_reginit(void) { }
#line 1 "cgo-generated-wrappers"
extern void replacement_scan_cb();
extern void replacement_scan_destroy_data();
extern void scalar_udf_callback();
extern void table_udf_bind_chunk();
extern void table_udf_bind_parallel_chunk();
extern void table_udf_bind_parallel_row();
extern void table_udf_bind_row();
extern void table_udf_chunk_callback();
extern void table_udf_init();
extern void table_udf_init_parallel();
extern void table_udf_local_init();
extern void table_udf_row_callback();
extern void udf_delete_callback();
void _cgoexp_b59e10b900ff_replacement_scan_destroy_data(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_replacement_scan_cb(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_scalar_udf_callback(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_table_udf_bind_row(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_table_udf_bind_chunk(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_table_udf_bind_parallel_row(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_table_udf_bind_parallel_chunk(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_table_udf_init(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_table_udf_init_parallel(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_table_udf_local_init(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_table_udf_row_callback(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_table_udf_chunk_callback(void* p __attribute__((unused))){}
void _cgoexp_b59e10b900ff_udf_delete_callback(void* p __attribute__((unused))){}
