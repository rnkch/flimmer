import { files, t } from './i18n.js';

// The add form: files or a link, never both. Files are uploaded with
// XMLHttpRequest (fetch() has no upload progress); a checked link fills
// #url-result/#url-actions via htmx, and its buttons post with htmx.
export function uploader() {
  return {
    mode: '', // '' | 'files' | 'url'
    busy: false,
    percent: 0,
    files: '',
    title: '', // page title of a checked link, shown as name placeholder

    pick(input) {
      const n = input.files.length;
      this.files = n === 0 ? '' : n === 1 ? input.files[0].name : files(n);
      if (n > 0) {
        this.clearLink(input.form);
        this.mode = 'files';
      } else if (this.mode === 'files') {
        this.mode = '';
      }
    },

    linkTyped(input) {
      if (!input.value) return;
      const file = input.form.querySelector('input[type=file]');
      if (file.value) {
        file.value = '';
        this.files = '';
      }
      if (this.mode === 'files') this.mode = '';
    },

    linkChecked(title) {
      this.title = title;
      this.mode = 'url';
    },

    clearLink(form) {
      form.querySelector('[name=url]').value = '';
      form.querySelector('#url-result').innerHTML = '';
      form.querySelector('#url-actions').innerHTML = '';
      this.title = '';
    },

    reset(form) {
      form.reset();
      this.clearLink(form);
      this.files = '';
      this.mode = '';
    },

    send(form) {
      const input = form.querySelector('input[type=file]');
      if (!input.files.length) return;
      this.busy = true;
      this.percent = 0;
      const data = new FormData(form);
      data.delete('url');
      const xhr = new XMLHttpRequest();
      xhr.open('POST', form.action);
      xhr.setRequestHeader('HX-Request', 'true');
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) this.percent = Math.round((e.loaded / e.total) * 100);
      };
      xhr.onload = () => {
        this.busy = false;
        if (xhr.status >= 200 && xhr.status < 300) {
          this.reset(form);
          htmx.trigger(document.body, 'jobsChanged');
          toast(t('Uploaded, processing'), 'success');
        } else {
          toast(xhr.responseText || t('Upload failed (HTTP {0})', xhr.status), 'danger');
        }
      };
      xhr.onerror = () => {
        this.busy = false;
        toast(t('Connection lost'), 'danger');
      };
      xhr.send(data);
    },
  };
}

function toast(message, variant) {
  document.dispatchEvent(new CustomEvent('showToast', { detail: { message, variant } }));
}
