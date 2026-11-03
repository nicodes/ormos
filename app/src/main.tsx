import "./index.css";
/* @refresh reload */
import { render } from "solid-js/web";
import App from "./App";

render(() => <App />, document.getElementById("root")!);

if (new URLSearchParams(location.search).get("layout-report") === "1") {
  void import("./viewportReport").then(({ installViewportReport }) => {
    const dispose = installViewportReport();
    import.meta.hot?.dispose(dispose);
  });
}
