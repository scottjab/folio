// Browser entry point.
//
// Everything lives in app.ts; this file exists so that importing the app in a
// test does not start it.

import "./style.css";
import { App } from "./app";

void new App().start();
