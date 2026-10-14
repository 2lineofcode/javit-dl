export function call(method, ...args) {
  const app = window.go?.gui?.App;
  if (!app) return Promise.reject(new Error('Desktop connection unavailable. Run ./build.sh, then ./kuraku gui.'));
  return app[method](...args);
}
