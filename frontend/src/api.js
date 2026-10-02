export function call(method, ...args) {
  const app = window.go?.gui?.App;
  if (!app) return Promise.reject(new Error('Desktop connection unavailable. Run ./build.sh, then ./javit_dl gui.'));
  return app[method](...args);
}
