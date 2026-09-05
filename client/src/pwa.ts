export const PWA_UPDATE_EVENT = 'wmux:pwa-update';

let reloading = false;

function announceUpdate(registration: ServiceWorkerRegistration): void {
  window.dispatchEvent(new CustomEvent(PWA_UPDATE_EVENT, { detail: registration }));
}

export function registerPwa(): void {
  if (!import.meta.env.PROD || !('serviceWorker' in navigator)) return;

  const register = () => {
    void navigator.serviceWorker
      .register('/sw.js')
      .then((registration) => {
        if (registration.waiting) announceUpdate(registration);
        registration.addEventListener('updatefound', () => {
          const worker = registration.installing;
          if (!worker) return;
          worker.addEventListener('statechange', () => {
            if (worker.state === 'installed' && navigator.serviceWorker.controller) announceUpdate(registration);
          });
        });
      })
      .catch(() => {
        // Offline use and terminal access do not depend on PWA installation.
      });
  };
  if (document.readyState === 'complete') register();
  else window.addEventListener('load', register, { once: true });
}

export function activateUpdate(registration: ServiceWorkerRegistration): void {
  navigator.serviceWorker.addEventListener('controllerchange', () => {
    if (reloading) return;
    reloading = true;
    window.location.reload();
  });
  if (registration.waiting) registration.waiting.postMessage({ type: 'SKIP_WAITING' });
  else window.location.reload();
}
