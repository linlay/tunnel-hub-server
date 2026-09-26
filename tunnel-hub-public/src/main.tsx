import React from 'react';
import ReactDOM from 'react-dom/client';
import { App } from './App';
import { applyPublicSiteTitle } from './runtimeConfig';
import './styles.css';

void applyPublicSiteTitle().catch((error: unknown) => {
  console.error('Failed to apply runtime site title', error);
});

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
);
