import { createApp } from "vue";
import "element-plus/es/components/message/style/css";
import "element-plus/es/components/message-box/style/css";
import "element-plus/es/components/notification/style/css";

import App from "./App.vue";
import router from "./router/index.js";
import "./styles/index.css";

createApp(App).use(router).mount("#app");
