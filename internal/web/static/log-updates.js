(() => {
  const responses = new WeakMap();
  const clock = new Intl.DateTimeFormat(undefined, {
    hour: 'numeric', minute: '2-digit', second: '2-digit'
  });

  function connectionFor(target) {
    if (!target?.matches('[data-log-refresh]')) return null;
    return target.closest('[data-log-panel]')?.querySelector('[data-log-connection]');
  }

  function show(connection, state, text) {
    connection.dataset.state = state;
    const message = connection.querySelector('[data-log-message]');
    if (message.textContent !== text) message.textContent = text;
  }

  document.addEventListener('htmx:beforeSwap', ({ detail }) => {
    const { target, xhr, serverResponse } = detail;
    const connection = connectionFor(target);
    if (!connection) return;

    const result = xhr.getResponseHeader('X-Nori-Log-Refresh');
    if (xhr.status !== 200 || result !== 'ok') {
      // Keep the last good output, including when a login page is returned.
      detail.shouldSwap = false;
      let message = 'Updates unavailable · retrying';
      if (result === 'docker-error') {
        message = 'Could not read container logs · retrying';
      } else if (xhr.responseURL && new URL(xhr.responseURL).pathname === '/login') {
        message = 'Session expired · reload to sign in';
      }
      show(connection, 'unavailable', message);
      return;
    }

    show(connection, 'connected', 'Receiving updates');
    const checked = connection.querySelector('[data-log-checked]');
    checked.textContent = 'Last checked ' + clock.format(new Date());
    checked.hidden = false;

    if (responses.get(connection)?.html === serverResponse) {
      detail.shouldSwap = false;
      return;
    }
    const box = target.querySelector('.log-box');
    responses.set(connection, {
      html: serverResponse,
      container: target.dataset.logContainer,
      scrollTop: box?.scrollTop ?? 0,
      scrollLeft: box?.scrollLeft ?? 0
    });
  });

  document.addEventListener('htmx:afterSwap', ({ target }) => {
    const connection = connectionFor(target);
    if (!connection) return;
    const previous = responses.get(connection);
    if (previous && previous.container === target.dataset.logContainer) {
      target.querySelector('.log-box')?.scrollTo(previous.scrollLeft, previous.scrollTop);
    }
    if (!target.hasAttribute('hx-get')) {
      show(connection, 'connected', 'Updates complete');
      responses.delete(connection);
    }
  });

  function connectionLost({ detail }) {
    const connection = connectionFor(detail.target);
    if (!connection) return;
    show(connection, 'unavailable', navigator.onLine
      ? 'Connection interrupted · retrying'
      : 'Browser offline · retrying');
  }

  document.addEventListener('htmx:sendError', connectionLost);
  document.addEventListener('htmx:timeout', connectionLost);
})();
