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
extern void authorizerTrampoline();
extern void callbackTrampoline();
extern void commitHookTrampoline();
extern void compareTrampoline();
extern void doneTrampoline();
extern void rollbackHookTrampoline();
extern void stepTrampoline();
extern void updateHookTrampoline();
void _cgoexp_6c9441af552b_callbackTrampoline(void* p __attribute__((unused))){}
void _cgoexp_6c9441af552b_stepTrampoline(void* p __attribute__((unused))){}
void _cgoexp_6c9441af552b_doneTrampoline(void* p __attribute__((unused))){}
void _cgoexp_6c9441af552b_compareTrampoline(void* p __attribute__((unused))){}
void _cgoexp_6c9441af552b_commitHookTrampoline(void* p __attribute__((unused))){}
void _cgoexp_6c9441af552b_rollbackHookTrampoline(void* p __attribute__((unused))){}
void _cgoexp_6c9441af552b_updateHookTrampoline(void* p __attribute__((unused))){}
void _cgoexp_6c9441af552b_authorizerTrampoline(void* p __attribute__((unused))){}
void _cgoexp_6c9441af552b_preUpdateHookTrampoline(void* p __attribute__((unused))){}
