import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  build: {
    lib: {
      entry: 'src/index.ts',
      formats: ['es'],
      fileName: 'index',
    },
    outDir: 'dist',
    rollupOptions: {
      // React 由核心前端提供，插件不打包
      external: ['react', 'react-dom', 'react/jsx-runtime'],
    },
  },
});
