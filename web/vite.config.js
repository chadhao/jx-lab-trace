import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 构建产物落 web/dist；scripts/build.sh 再拷进 internal/webui/dist 供 //go:embed 内嵌。
// ★ 刻意不配置 dev server：按 docs/05，本机不跑服务，联调一律在测试服务器上进行。
export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
})
