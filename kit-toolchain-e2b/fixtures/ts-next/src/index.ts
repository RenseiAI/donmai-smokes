// Minimal TS source so `npm run build` (tsc) has something real to compile and
// `npm test` (node dist/index.js) produces an asserted marker line.
function greet(name: string): string {
  return `kit-toolchain-e2b: build OK for ${name}`;
}

const msg: string = greet("ts-next");
// The proof asserts this exact marker appears in the sandbox test output.
console.log("KIT_TOOLCHAIN_BUILD_TEST_OK:" + msg);
