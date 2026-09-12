import '@testing-library/jest-dom/vitest'

// Node 25 ships an unflagged Web Storage `localStorage` global that shadows the one
// jsdom installs, and the replacement has none of Storage's methods: every test
// whose beforeEach calls localStorage.clear() dies with "not a function". That is
// 203 of 300 tests here, all reported as assertion failures in code the run never
// reached, and the message points at the test rather than at the runtime. Only
// localStorage is affected — sessionStorage still comes from jsdom — so the shape of
// the breakage gives no hint either. This only ever hits a developer whose PATH
// resolves to a Node newer than the supported range in package.json — exactly the
// case where a wall of red is least likely to be read as an environment problem.
// Failing here names the cause once instead.
//
// process is reached through globalThis rather than as a bare global because this
// package has no @types/node: tsconfig.app.json declares only vite/client, and
// `process.version` alone fails tsc -b with TS2591, which takes make build down
// with it. Pulling Node's whole type surface into a browser bundle's typecheck to
// read one string is the wrong trade.
if (typeof localStorage.clear !== 'function') {
  const version = (globalThis as { process?: { version?: string } }).process?.version ?? 'unknown'
  throw new Error(
    `This Node (${version}) provides a localStorage global that shadows jsdom's and lacks Storage's methods. ` +
      'Run the web tests on Node 22-24, the range package.json declares; CI pins 24.',
  )
}
