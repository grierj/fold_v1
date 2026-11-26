import { test } from "@playwright/test";
import { adminRoomReadTests } from "./admin-room-reads";

test.use({ viewport: { width: 390, height: 844 } });

adminRoomReadTests({ path: "/admin/rooms", otherRead: "/admin/room-requests", evidence: "admin-room-catalog" });
